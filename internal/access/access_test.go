package access

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
)

// records reads what the registry holds: hashes, never codes.
func records(r *Registry) []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneRecords(r.records)
}

// counterRand gives predictable, distinct salts.
type counterRand struct{ n byte }

func (c *counterRand) Read(p []byte) (int, error) {
	for i := range p {
		c.n++
		p[i] = c.n
	}
	return len(p), nil
}

type failingRand struct{}

func (failingRand) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

type store struct {
	saved [][]Record
	fail  error
}

func (s *store) persist(r []Record) error {
	if s.fail != nil {
		return s.fail
	}
	s.saved = append(s.saved, r)
	return nil
}

func testConfig() Config {
	c := DefaultConfig()
	c.Iterations = minIterations // fast tests; the default cost is for the hub
	c.Rand = &counterRand{}
	return c
}

func newRegistry(t *testing.T) (*Registry, *store) {
	t.Helper()
	s := &store{}
	r, err := New(testConfig(), nil, s.persist)
	if err != nil {
		t.Fatal(err)
	}
	return r, s
}

func TestSetThenVerifyFindsWhoseCodeItIs(t *testing.T) {
	r, _ := newRegistry(t)
	for user, code := range map[string]string{"u_owner": "482915", "u_guard": "73190264", "u_manager": "9051"} {
		if err := r.Set(user, code); err != nil {
			t.Fatalf("%s: %v", user, err)
		}
	}
	for user, code := range map[string]string{"u_owner": "482915", "u_guard": "73190264", "u_manager": "9051"} {
		if got, ok := r.Verify(code); !ok || got != user {
			t.Errorf("Verify(%s) = %q %v, want %q", code, got, ok, user)
		}
	}
}

func TestWrongCodesAreRefused(t *testing.T) {
	r, _ := newRegistry(t)
	r.Set("u_owner", "482915")
	for _, code := range []string{"482916", "48291", "4829150", "", "000000", "abcdef", "48 915", "\uff14\uff18\uff12\uff19\uff11\uff15"} {
		if user, ok := r.Verify(code); ok {
			t.Errorf("Verify(%q) accepted as %q", code, user)
		}
	}
}

func TestVerifyOnAnEmptyRegistry(t *testing.T) {
	r, _ := newRegistry(t)
	if _, ok := r.Verify("482915"); ok {
		t.Fatal("nobody has a code yet")
	}
}

func TestCodeFormat(t *testing.T) {
	r, _ := newRegistry(t)
	for _, code := range []string{"", "123", "12345678 ", "123456789", "48291a", "-48291", "48.915"} {
		if err := r.Set("u", code); !errors.Is(err, ErrInvalidCode) {
			t.Errorf("Set(%q) = %v, want ErrInvalidCode", code, err)
		}
	}
	if err := r.Set("u", "8403"); err != nil { // the shortest allowed
		t.Fatal(err)
	}
	if err := r.Set("v", "84039271"); err != nil { // the longest allowed
		t.Fatal(err)
	}
}

func TestWeakCodesAreRefused(t *testing.T) {
	r, _ := newRegistry(t)
	for _, code := range []string{"0000", "1111", "777777", "1234", "4321", "123456", "987654", "7890", "0987", "34567890"} {
		if err := r.Set("u", code); !errors.Is(err, ErrWeakCode) {
			t.Errorf("Set(%q) = %v, want ErrWeakCode", code, err)
		}
	}
	for _, code := range []string{"482915", "1357", "1212", "123465", "112233"} {
		if err := r.Set("u", code); err != nil {
			t.Errorf("Set(%q) = %v, want it accepted", code, err)
		}
	}
}

func TestAtMostFiveCodes(t *testing.T) {
	r, _ := newRegistry(t)
	codes := []string{"482915", "730264", "905137", "261804", "519372"}
	for i, c := range codes {
		if err := r.Set(fmt.Sprintf("u_%d", i), c); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Set("u_5", "846029"); !errors.Is(err, ErrTooManyUsers) {
		t.Fatalf("sixth person: %v", err)
	}
	// Someone who already has a code can change it even when the site is full.
	if err := r.Set("u_0", "846029"); err != nil {
		t.Fatalf("changing a code: %v", err)
	}
	if _, ok := r.Verify("482915"); ok {
		t.Fatal("the old code must stop working")
	}
	if u, ok := r.Verify("846029"); !ok || u != "u_0" {
		t.Fatalf("%q %v", u, ok)
	}
	if n := len(r.Users()); n != 5 {
		t.Fatalf("%d users", n)
	}
}

func TestACodeIdentifiesOnePerson(t *testing.T) {
	r, _ := newRegistry(t)
	r.Set("u_owner", "482915")
	if err := r.Set("u_guard", "482915"); !errors.Is(err, ErrCodeTaken) {
		t.Fatalf("got %v", err)
	}
	if err := r.Set("u_owner", "482915"); err != nil {
		t.Fatalf("a person may set their own code again: %v", err)
	}
}

func TestUserIdentifiers(t *testing.T) {
	r, _ := newRegistry(t)
	for _, u := range []string{"", strings.Repeat("x", maxUserLen+1)} {
		if err := r.Set(u, "482915"); !errors.Is(err, ErrInvalidUser) {
			t.Errorf("Set(%q) = %v", u, err)
		}
	}
	if err := r.Set(strings.Repeat("x", maxUserLen), "482915"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveStopsTheCodeAndOnlyThatOne(t *testing.T) {
	r, s := newRegistry(t)
	r.Set("u_owner", "482915")
	r.Set("u_guard", "730264")
	if err := r.Remove("u_guard"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Verify("730264"); ok {
		t.Fatal("the removed person's code must stop working")
	}
	if u, ok := r.Verify("482915"); !ok || u != "u_owner" {
		t.Fatal("the other codes must keep working")
	}
	if !slices.Equal(r.Users(), []string{"u_owner"}) {
		t.Fatalf("%v", r.Users())
	}
	if last := s.saved[len(s.saved)-1]; len(last) != 1 {
		t.Fatalf("the removal must be saved: %v", last)
	}
	if err := r.Remove("u_guard"); !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("got %v", err)
	}
	// The freed seat and the freed code can be used again.
	if err := r.Set("u_new", "730264"); err != nil {
		t.Fatal(err)
	}
}

func TestEveryChangeIsSavedBeforeItTakesEffect(t *testing.T) {
	r, s := newRegistry(t)
	s.fail = errors.New("disk full")

	if err := r.Set("u_owner", "482915"); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("got %v", err)
	}
	if _, ok := r.Verify("482915"); ok || len(r.Users()) != 0 {
		t.Fatal("a code that could not be saved must not be accepted: it would vanish at the next restart")
	}

	s.fail = nil
	r.Set("u_owner", "482915")
	s.fail = errors.New("disk full")
	if err := r.Set("u_owner", "730264"); err == nil {
		t.Fatal("want an error")
	}
	if u, ok := r.Verify("482915"); !ok || u != "u_owner" {
		t.Fatal("after a failed change, the old code must still work")
	}
	if err := r.Remove("u_owner"); err == nil {
		t.Fatal("want an error")
	}
	if _, ok := r.Verify("482915"); !ok {
		t.Fatal("after a failed removal, the code must still work")
	}
}

func TestSavedRecordsComeBackAfterARestart(t *testing.T) {
	r, s := newRegistry(t)
	r.Set("u_owner", "482915")
	r.Set("u_guard", "730264")

	// What the application would write to the disk, and read back.
	b, err := json.Marshal(s.saved[len(s.saved)-1])
	if err != nil {
		t.Fatal(err)
	}
	var back []Record
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	again, err := New(testConfig(), back, (&store{}).persist)
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := again.Verify("730264"); !ok || u != "u_guard" {
		t.Fatalf("%q %v", u, ok)
	}
	if _, ok := again.Verify("730265"); ok {
		t.Fatal("wrong code accepted after a restart")
	}
}

func TestNothingSavedOrPrintedContainsTheCode(t *testing.T) {
	r, s := newRegistry(t)
	const code = "482915"
	if err := r.Set("u_owner", code); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s.saved)
	for what, text := range map[string]string{
		"saved records":  string(b),
		"%v of registry": fmt.Sprintf("%v %+v %#v", r.Users(), r, r),
	} {
		if strings.Contains(text, code) {
			t.Errorf("%s contains the code", what)
		}
	}
	// Errors never repeat the code either.
	for _, err := range []error{r.Set("u", "1111"), r.Set("u", "12a456"), r.Set("v", code), r.Remove("nobody")} {
		if err != nil && strings.Contains(err.Error(), code) {
			t.Errorf("error %q contains the code", err)
		}
	}
	if err := (&Registry{cfg: testConfig(), persist: func([]Record) error { return errors.New("x" + code) }}).Set("u", "730264"); err != nil && strings.Contains(err.Error(), "730264") {
		t.Error("a persistence error must not drag the code along")
	}
}

func TestEachCodeGetsItsOwnSaltAndTheRecordsHoldNoClearCode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Iterations = minIterations
	s := &store{}
	r, err := New(cfg, nil, s.persist) // real random source
	if err != nil {
		t.Fatal(err)
	}
	r.Set("a", "482915")
	r.Set("a", "482915") // the same code again, for the same person
	recs := records(r)
	prev := s.saved[0][0]
	if bytes.Equal(prev.Salt, recs[0].Salt) || bytes.Equal(prev.Hash, recs[0].Hash) {
		t.Fatal("a new salt is needed every time: the same code must not give the same hash")
	}
	if recs[0].Algo != algoPBKDF2 || recs[0].Iterations != minIterations || len(recs[0].Salt) != saltBytes || len(recs[0].Hash) != hashBytes {
		t.Fatalf("%+v", recs[0])
	}
}

func TestNoSaltNoCode(t *testing.T) {
	cfg := testConfig()
	cfg.Rand = failingRand{}
	r, err := New(cfg, nil, (&store{}).persist)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Set("u", "482915"); err == nil {
		t.Fatal("without randomness the salt would be predictable: refuse")
	}
	cfg.Rand = io.LimitReader(&counterRand{}, 3) // too short
	r, _ = New(cfg, nil, (&store{}).persist)
	if err := r.Set("u", "482915"); err == nil {
		t.Fatal("a short salt must be refused")
	}
}

func TestNewCopiesItsInput(t *testing.T) {
	r, _ := newRegistry(t)
	r.Set("u_owner", "482915")
	in := records(r)
	again, _ := New(testConfig(), in, (&store{}).persist)
	in[0].Hash[0] ^= 0xff
	if _, ok := again.Verify("482915"); !ok {
		t.Fatal("changing the input must not change the registry")
	}
}

func TestNewRefusesDamagedRecords(t *testing.T) {
	r, _ := newRegistry(t)
	r.Set("a", "482915")
	r.Set("b", "730264")
	good := records(r)

	mutate := map[string]func([]Record) []Record{
		"duplicate user":    func(x []Record) []Record { x[1].User = x[0].User; return x },
		"empty user":        func(x []Record) []Record { x[0].User = ""; return x },
		"unknown algorithm": func(x []Record) []Record { x[0].Algo = "md5"; return x },
		"few iterations":    func(x []Record) []Record { x[0].Iterations = 1; return x },
		"short salt":        func(x []Record) []Record { x[0].Salt = x[0].Salt[:4]; return x },
		"short hash":        func(x []Record) []Record { x[0].Hash = x[0].Hash[:8]; return x },
		"too many": func(x []Record) []Record {
			for i := 0; i < 5; i++ {
				c := x[0]
				c.User = fmt.Sprintf("extra-%d", i)
				x = append(x, c)
			}
			return x
		},
	}
	for name, f := range mutate {
		if _, err := New(testConfig(), f(cloneRecords(good)), (&store{}).persist); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Config){
		"no users":       func(c *Config) { c.MaxUsers = 0 },
		"3 digits":       func(c *Config) { c.MinDigits = 3 },
		"max below min":  func(c *Config) { c.MaxDigits = 3 },
		"weak hash cost": func(c *Config) { c.Iterations = 1000 },
	}
	for name, mutate := range bad {
		c := DefaultConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if _, err := New(c, nil, (&store{}).persist); err == nil {
			t.Errorf("%s: New must refuse", name)
		}
	}
	if _, err := New(DefaultConfig(), nil, nil); err == nil {
		t.Error("persist is required")
	}
}

func TestDefaultsMatchSpecification(t *testing.T) {
	c := DefaultConfig()
	if c.MaxUsers != 5 || c.MinDigits != 4 || c.Iterations < 100_000 {
		t.Fatalf("defaults differ from docs/security-install.md: %+v", c)
	}
}

func TestSafeForConcurrentUse(t *testing.T) {
	r, _ := newRegistry(t)
	r.Set("u_owner", "482915")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				r.Verify("482915")
				r.Users()
			}
		}()
		go func(i int) {
			defer wg.Done()
			r.Set(fmt.Sprintf("tmp_%d", i%2), fmt.Sprintf("%06d", 480000+i*37))
			r.Remove(fmt.Sprintf("tmp_%d", i%2))
		}(i)
	}
	wg.Wait()
	if u, ok := r.Verify("482915"); !ok || u != "u_owner" {
		t.Fatal("the owner's code must survive")
	}
}

// The hash cost on this machine, to tune Iterations on the hub: run
// go test -bench Verify ./internal/access on the Raspberry Pi.
func BenchmarkVerifyFiveCodes(b *testing.B) {
	cfg := DefaultConfig()
	s := &store{}
	r, _ := New(cfg, nil, s.persist)
	for i, c := range []string{"482915", "730264", "905137", "261804", "519372"} {
		if err := r.Set(fmt.Sprintf("u_%d", i), c); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Verify("000001")
	}
}

func TestWeakDetectorDirectly(t *testing.T) {
	cases := map[string]bool{"0000": true, "1234": true, "9876": true, "8901": true, "1243": false, "1357": false, "5550": false}
	for code, want := range cases {
		if got := weak(code); got != want {
			t.Errorf("weak(%q) = %v, want %v", code, got, want)
		}
	}
}

func TestReloadReplacesTheCodesWithTheSavedOnes(t *testing.T) {
	a, sa := newRegistry(t) // the registry of a running hub
	a.Set("u_owner", "482915")

	// Another process (the installer's tool) saves a different set.
	b, sb := newRegistry(t)
	b.Set("u_owner", "730264")
	b.Set("u_guard", "905137")
	saved := sb.saved[len(sb.saved)-1]

	if err := a.Reload(saved); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Verify("482915"); ok {
		t.Fatal("the old code must stop working")
	}
	if u, ok := a.Verify("905137"); !ok || u != "u_guard" {
		t.Fatalf("%q %v", u, ok)
	}
	if len(sa.saved) != 1 {
		t.Fatal("a reload must not write anything: the records are already on disk")
	}
	// Damaged records are refused and leave the codes in use alone.
	bad := cloneRecords(saved)
	bad[0].Algo = "md5"
	if err := a.Reload(bad); err == nil {
		t.Fatal("want an error")
	}
	if _, ok := a.Verify("905137"); !ok {
		t.Fatal("a refused reload must change nothing")
	}
	dup := cloneRecords(saved)
	dup[1].User = dup[0].User
	if err := a.Reload(dup); err == nil {
		t.Fatal("duplicate users must be refused")
	}
	tooMany := cloneRecords(saved)
	for i := 0; i < 5; i++ {
		c := tooMany[0]
		c.User = fmt.Sprintf("x%d", i)
		tooMany = append(tooMany, c)
	}
	if err := a.Reload(tooMany); err == nil {
		t.Fatal("too many codes must be refused")
	}
	if err := a.Reload(nil); err != nil || len(a.Users()) != 0 {
		t.Fatal("reloading an empty set removes every code")
	}
}
