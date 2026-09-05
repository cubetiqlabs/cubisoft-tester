package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestSummarize(t *testing.T) {
	s := summarize([]float64{10, 20, 30, 40, 100})
	if !near(s.Min, 10) || !near(s.Max, 100) || !near(s.Avg, 40) {
		t.Fatalf("min/max/avg wrong: %+v", s)
	}
	if !near(s.P50, 30) || !near(s.P95, 100) || !near(s.P99, 100) {
		t.Fatalf("percentiles wrong: %+v", s)
	}
	// Jitter must follow arrival order, not sorted order: |10|+|10|+|10|+|60| / 4.
	if !near(s.Jitter, 22.5) {
		t.Fatalf("jitter = %v, want 22.5", s.Jitter)
	}
	if got := summarize(nil); got != (Stats{}) {
		t.Fatalf("empty input should give zero Stats, got %+v", got)
	}
	if one := summarize([]float64{7}); !near(one.P99, 7) || !near(one.Jitter, 0) {
		t.Fatalf("single sample: %+v", one)
	}
}

func TestOkSamplesDropsTimeouts(t *testing.T) {
	got := okSamples([]float64{5, -1, 7})
	if len(got) != 2 || !near(got[0], 5) || !near(got[1], 7) {
		t.Fatalf("got %v", got)
	}
}

func TestDSN(t *testing.T) {
	c := Config{Host: "db.example.com", Port: 3307, User: "u", Password: "p@ss", Database: "app", TLS: "skip-verify"}
	dsn := c.dsn()
	for _, want := range []string{"u:p@ss@tcp(db.example.com:3307)/app", "tls=skip-verify", "interpolateParams=true", "timeout=10s"} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("dsn %q missing %q", dsn, want)
		}
	}
	if _, err := mysql.ParseDSN(dsn); err != nil {
		t.Fatalf("driver rejects our own DSN: %v", err)
	}
	// An IPv6 literal host must still produce a parseable address.
	v6 := Config{Host: "::1", User: "u"}.dsn()
	if !strings.Contains(v6, "tcp([::1]:3306)") {
		t.Fatalf("ipv6 host not bracketed: %s", v6)
	}
	// An unknown TLS value must fail closed to plaintext rather than to a bad DSN.
	if bad := (Config{Host: "h", User: "u", TLS: "nonsense"}).dsn(); !strings.Contains(bad, "tls=false") {
		t.Fatalf("unknown tls mode not normalised: %s", bad)
	}
}

func TestHint(t *testing.T) {
	if hint(nil) != "" {
		t.Fatal("nil error should have no hint")
	}
	if h := hint(&mysql.MySQLError{Number: 1045}); !strings.Contains(h, "password") {
		t.Fatalf("1045 hint = %q", h)
	}
	if h := hint(errors.New("dial tcp 1.2.3.4:3306: connect: connection refused")); !strings.Contains(h, "nothing is listening") {
		t.Fatalf("refused hint = %q", h)
	}
	if h := hint(errors.New("something nobody has ever seen")); h != "" {
		t.Fatalf("unknown error should have no hint, got %q", h)
	}
}

// embeddedSeq is the one piece of traceroute that silently breaks everything if
// it is wrong: get it wrong and every hop reads as a timeout.
func TestEmbeddedSeq(t *testing.T) {
	original, err := (&icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Body: &icmp.Echo{ID: 1234, Seq: 4242, Data: []byte("wails-mysql-trace")},
	}).Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	// A router quotes the IPv4 header of the packet it dropped, then its payload.
	iphdr := []byte{0x45, 0, 0, 0x3c, 0, 0, 0, 0, 1, 1, 0, 0, 10, 0, 0, 1, 10, 0, 0, 2}
	if got := embeddedSeq(append(iphdr, original...)); got != 4242 {
		t.Fatalf("embeddedSeq = %d, want 4242", got)
	}
	if got := embeddedSeq([]byte{1, 2, 3}); got != -1 {
		t.Fatalf("truncated packet should give -1, got %d", got)
	}
}

func TestBiggestJump(t *testing.T) {
	hops := []Hop{{TTL: 1, AvgMs: 1}, {TTL: 2, AvgMs: 2}, {TTL: 3, AvgMs: -1}, {TTL: 4, AvgMs: 90}, {TTL: 5, AvgMs: 92}}
	if got := biggestJump(hops); got != 4 {
		t.Fatalf("biggestJump = %d, want 4 (the ocean crossing)", got)
	}
	if got := biggestJump(nil); got != 0 {
		t.Fatalf("no hops should give 0, got %d", got)
	}
}

func TestHumanBytes(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{{512, "512 B"}, {1024, "1.0 KiB"}, {4 << 20, "4.0 MiB"}, {1 << 30, "1.0 GiB"}} {
		if got := humanBytes(c.in); got != c.want {
			t.Fatalf("humanBytes(%d) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestRandPayloadIsEscapeFree(t *testing.T) {
	p := randPayload(4096)
	if len(p) != 4096 {
		t.Fatalf("length = %d", len(p))
	}
	// Quotes or backslashes would inflate the interpolated statement and skew
	// every throughput number the speed test reports.
	if strings.ContainsAny(p, "'\"\\\x00\n\r") {
		t.Fatal("payload contains characters the driver would escape")
	}
}

func TestRatePhase(t *testing.T) {
	p := Phase{Rows: 1000, Bytes: 10 << 20, DurationMs: 2000}
	rate(&p)
	if !near(p.RowsPerSec, 500) || !near(p.MiBPerSec, 5) {
		t.Fatalf("rate = %+v", p)
	}
	zero := Phase{Rows: 5}
	rate(&zero)
	if zero.RowsPerSec != 0 {
		t.Fatal("zero duration must not divide by zero")
	}
}

func TestWarningsFlagsTheDangerousDefaults(t *testing.T) {
	si := ServerInfo{Flavor: "MySQL", Version: "5.6.51", Major: 5, Minor: 6, Supported: true,
		ReadOnly: true, MaxConnections: 100, ThreadsConnected: 95, WaitTimeoutSec: 30,
		MaxAllowedPacket: 1 << 20, SQLMode: "NO_ENGINE_SUBSTITUTION"}
	got := strings.Join(warnings(si, 500, 10), "\n")
	for _, want := range []string{"read_only", "near the limit", "wait_timeout", "max_allowed_packet", "not encrypted", "STRICT", "end of life", "skip_name_resolve"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing warning about %q in:\n%s", want, got)
		}
	}
	healthy := ServerInfo{Flavor: "MySQL", Version: "8.0.36", Major: 8, Minor: 0, Supported: true,
		MaxConnections: 500, ThreadsConnected: 10, WaitTimeoutSec: 28800,
		MaxAllowedPacket: 64 << 20, SQLMode: "STRICT_TRANS_TABLES", TLSCipher: "TLS_AES_256_GCM_SHA384"}
	if w := warnings(healthy, 20, 15); len(w) != 0 {
		t.Fatalf("healthy server should produce no warnings, got %v", w)
	}
}

/* ---------- profiles ---------- */

// withTempConfigDir points os.UserConfigDir at a scratch directory so these
// tests never touch the real profile file.
func withTempConfigDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	switch runtime.GOOS {
	case "darwin":
		t.Setenv("HOME", dir)
	case "windows":
		t.Setenv("AppData", dir)
	default:
		t.Setenv("XDG_CONFIG_HOME", dir)
	}
	path, err := profilesPath()
	if err != nil || !strings.HasPrefix(path, dir) {
		t.Fatalf("profilesPath %q is not inside the temp dir %q (err %v)", path, dir, err)
	}
}

func sampleProfile(name string, savePassword bool) SaveProfileRequest {
	return SaveProfileRequest{Name: name, SavePassword: savePassword, Config: Config{
		Host: "db.example.com", Port: 3306, User: "app", Password: "s3cret", Database: "shop",
	}}
}

func TestProfilesPlaintextDropsPassword(t *testing.T) {
	withTempConfigDir(t)
	tester := &Tester{}

	// Asking to keep a password in a plaintext file must be refused outright,
	// not silently honoured.
	if err := tester.SaveProfile(sampleProfile("prod", true)); err == nil {
		t.Fatal("saving a password into an unencrypted vault was allowed")
	}
	if err := tester.SaveProfile(sampleProfile("prod", false)); err != nil {
		t.Fatal(err)
	}
	got, err := tester.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "prod" {
		t.Fatalf("got %+v", got)
	}
	// An unencrypted vault is a plain file on disk, so it must not hold secrets.
	if got[0].Config.Password != "" {
		t.Fatal("password was written to an unencrypted profile file")
	}
	if got[0].UpdatedAt == "" {
		t.Fatal("UpdatedAt not stamped")
	}

	path, _ := profilesPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "s3cret") {
		t.Fatal("password found in the file on disk")
	}
}

func TestProfilesEncryptionRoundTrip(t *testing.T) {
	withTempConfigDir(t)
	tester := &Tester{}

	if err := tester.SetProfilesSecret("correct horse"); err != nil {
		t.Fatal(err)
	}
	if err := tester.SaveProfile(sampleProfile("prod", true)); err != nil {
		t.Fatal(err)
	}
	// Opting out still drops the password, even in an encrypted vault.
	if err := tester.SaveProfile(sampleProfile("staging", false)); err != nil {
		t.Fatal(err)
	}

	got, err := tester.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 profiles, got %+v", got)
	}
	byName := map[string]Profile{}
	for _, p := range got {
		byName[p.Name] = p
	}
	if byName["prod"].Config.Password != "s3cret" {
		t.Fatalf("password should survive in an encrypted vault: %+v", byName["prod"])
	}
	if byName["staging"].Config.Password != "" {
		t.Fatal("password kept despite savePassword=false")
	}

	path, _ := profilesPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"s3cret", "db.example.com", "prod"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%q is readable in the encrypted file", secret)
		}
	}

	// A fresh session starts locked and stays locked until the key is right.
	fresh := &Tester{}
	if _, err := fresh.ListProfiles(); !errors.Is(err, errLocked) {
		t.Fatalf("locked store should refuse to list, got %v", err)
	}
	if err := fresh.UnlockProfiles("wrong horse"); err == nil {
		t.Fatal("wrong key was accepted")
	}
	if err := fresh.UnlockProfiles("correct horse"); err != nil {
		t.Fatal(err)
	}
	got, err = fresh.ListProfiles()
	if err != nil || len(got) != 2 {
		t.Fatalf("unlock did not restore the profiles: %+v %v", got, err)
	}
}

func TestProfilesRemovingEncryptionClearsPasswords(t *testing.T) {
	withTempConfigDir(t)
	tester := &Tester{}
	if err := tester.SetProfilesSecret("key"); err != nil {
		t.Fatal(err)
	}
	if err := tester.SaveProfile(sampleProfile("prod", true)); err != nil {
		t.Fatal(err)
	}
	if err := tester.SetProfilesSecret(""); err != nil {
		t.Fatal(err)
	}
	got, err := tester.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Config.Password != "" {
		t.Fatalf("password survived decryption: %+v", got)
	}
}

func TestProfilesTamperedFileIsRejected(t *testing.T) {
	withTempConfigDir(t)
	tester := &Tester{}
	if err := tester.SetProfilesSecret("key"); err != nil {
		t.Fatal(err)
	}
	if err := tester.SaveProfile(sampleProfile("prod", true)); err != nil {
		t.Fatal(err)
	}

	path, _ := profilesPath()
	raw, _ := os.ReadFile(path)
	var v vault
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	// Flip one ciphertext byte: GCM must refuse rather than hand back garbage.
	payload, _ := base64.StdEncoding.DecodeString(v.Payload)
	payload[len(payload)/2] ^= 0x01
	v.Payload = base64.StdEncoding.EncodeToString(payload)
	edited, _ := json.Marshal(v)
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := (&Tester{}).UnlockProfiles("key"); err == nil {
		t.Fatal("tampered vault was accepted")
	}
}

func TestSaveProfileNameValidation(t *testing.T) {
	withTempConfigDir(t)
	if err := (&Tester{}).SaveProfile(SaveProfileRequest{Name: "   "}); err == nil {
		t.Fatal("blank name was accepted")
	}
	// Names render straight into the picker, so keep control characters out.
	if err := (&Tester{}).SaveProfile(SaveProfileRequest{Name: "bad\x00name"}); err == nil {
		t.Fatal("control character in name was accepted")
	}
}

func TestBackupArgsMapOptions(t *testing.T) {
	opts := BackupOptions{
		Config: Config{Database: "shop"}, Tables: []string{"orders", "users"},
		SchemaOnly: true, Routines: true, SingleTransaction: true,
	}
	got := strings.Join(opts.args("/tmp/x.cnf"), " ")
	for _, want := range []string{"--defaults-file=/tmp/x.cnf", "--no-data", "--routines", "--single-transaction", "--skip-triggers", "--skip-add-drop-table", "shop orders users"} {
		if !strings.Contains(got, want) {
			t.Fatalf("args %q missing %q", got, want)
		}
	}
	// --defaults-file is ignored unless it is the first argument.
	if !strings.HasPrefix(got, "--defaults-file=") {
		t.Fatalf("defaults-file must come first: %q", got)
	}
}

func TestDefaultsFileIsPrivateAndHoldsThePassword(t *testing.T) {
	path, err := defaultsFile(Config{Host: "h", Port: 3307, User: "u", Password: "p@ss", TLS: "skip-verify"})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The whole point of the file is keeping the password off the command line,
	// so it must not be readable by anyone else.
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
	body, _ := os.ReadFile(path)
	for _, want := range []string{"[client]", "host=h", "port=3307", "user=u", "password=p@ss", "ssl-mode=REQUIRED"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("defaults file missing %q:\n%s", want, body)
		}
	}
}

func TestProfilesImportFromFile(t *testing.T) {
	withTempConfigDir(t)
	source := &Tester{}
	if err := source.SetProfilesSecret("export key"); err != nil {
		t.Fatal(err)
	}
	if err := source.SaveProfile(sampleProfile("prod", true)); err != nil {
		t.Fatal(err)
	}
	// Export under a key of its own, which is what carries the passwords.
	exported := filepath.Join(t.TempDir(), "exported.json")
	data, err := source.exportBytes("export key")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exported, data, 0o600); err != nil {
		t.Fatal(err)
	}

	// A dropped file has to answer "do I need a key?" before anything prompts.
	withTempConfigDir(t) // a different machine: empty vault
	target := &Tester{}
	encrypted, err := target.ImportEncrypted(exported)
	if err != nil || !encrypted {
		t.Fatalf("ImportEncrypted = %v, %v; want true", encrypted, err)
	}
	if _, err := target.ImportProfiles(exported, "wrong key", false); err == nil {
		t.Fatal("import accepted the wrong key")
	}

	names, err := target.ImportProfiles(exported, "export key", false)
	if err != nil || len(names) != 1 || names[0] != "prod" {
		t.Fatalf("import = %v, %v", names, err)
	}
	got, err := target.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "prod" {
		t.Fatalf("got %+v", got)
	}
	// The receiving vault is plaintext, so the imported password is dropped
	// rather than being written out in the clear.
	if got[0].Config.Password != "" {
		t.Fatal("password landed in an unencrypted vault")
	}

	// Import into an encrypted vault and it survives.
	if err := target.SetProfilesSecret("local key"); err != nil {
		t.Fatal(err)
	}
	if _, err := target.ImportProfiles(exported, "export key", false); err != nil {
		t.Fatal(err)
	}
	got, _ = target.ListProfiles()
	if len(got) != 1 || got[0].Config.Password != "s3cret" {
		t.Fatalf("password did not survive into an encrypted vault: %+v", got)
	}
}

func TestImportEncryptedIsFalseForPlaintext(t *testing.T) {
	withTempConfigDir(t)
	tester := &Tester{}
	if err := tester.SaveProfile(sampleProfile("prod", false)); err != nil {
		t.Fatal(err)
	}
	path, _ := profilesPath()
	// A plaintext export must not make the UI ask for a key it does not need.
	encrypted, err := tester.ImportEncrypted(path)
	if err != nil || encrypted {
		t.Fatalf("ImportEncrypted = %v, %v; want false", encrypted, err)
	}
	names, err := tester.ImportProfiles(path, "", false)
	if err != nil || len(names) != 1 {
		t.Fatalf("import without a key = %v, %v", names, err)
	}
}

func TestImportEncryptedRejectsNonsense(t *testing.T) {
	junk := filepath.Join(t.TempDir(), "notes.json")
	if err := os.WriteFile(junk, []byte(`{"hello":"world"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Tester{}).ImportEncrypted(junk); err == nil {
		t.Fatal("a file that is not a vault was accepted")
	}
}

func TestExportCarriesPasswordsOnlyWhenEncrypted(t *testing.T) {
	withTempConfigDir(t)
	tester := &Tester{}
	if err := tester.SetProfilesSecret("local key"); err != nil {
		t.Fatal(err)
	}
	if err := tester.SaveProfile(sampleProfile("prod", true)); err != nil {
		t.Fatal(err)
	}

	// A plaintext export is a readable file, so the password must not be in it.
	plain, err := tester.exportBytes("")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "s3cret") {
		t.Fatalf("password written to a plaintext export:\n%s", plain)
	}
	var v vault
	if err := json.Unmarshal(plain, &v); err != nil {
		t.Fatal(err)
	}
	if v.Encrypted || len(v.Profiles) != 1 || v.Profiles[0].Config.Password != "" {
		t.Fatalf("unexpected plaintext export: %+v", v)
	}

	// An encrypted export keeps them, under a key that is not the local one.
	sealed, err := tester.exportBytes("export key")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "s3cret") || strings.Contains(string(sealed), "prod") {
		t.Fatal("encrypted export leaks readable content")
	}
	out := filepath.Join(t.TempDir(), "e.json")
	if err := os.WriteFile(out, sealed, 0o600); err != nil {
		t.Fatal(err)
	}

	withTempConfigDir(t)
	target := &Tester{}
	if err := target.SetProfilesSecret("other machine key"); err != nil {
		t.Fatal(err)
	}
	// The local key and the export key are deliberately different.
	if _, err := target.ImportProfiles(out, "other machine key", false); err == nil {
		t.Fatal("the local key opened an export sealed with a different key")
	}
	if _, err := target.ImportProfiles(out, "export key", false); err != nil {
		t.Fatal(err)
	}
	got, _ := target.ListProfiles()
	if len(got) != 1 || got[0].Config.Password != "s3cret" {
		t.Fatalf("password did not travel with the export: %+v", got)
	}
}

func TestExportRefusesWhenEmpty(t *testing.T) {
	withTempConfigDir(t)
	if _, err := (&Tester{}).exportBytes(""); err == nil {
		t.Fatal("exported an empty vault")
	}
}
