package gsm

import (
	"testing"
	"time"
)

func TestNormalizeNumber(t *testing.T) {
	for raw, want := range map[string]string{
		"90 12 34 56":      "+22890123456",
		"90.12.34.56":      "+22890123456",
		"+228 90-12-34-56": "+22890123456",
		"0022890123456":    "+22890123456",
		"(+33) 6 12345678": "+33612345678",
		"070000001":        "+22870000001",
	} {
		if got, err := NormalizeNumber(raw, "228"); err != nil || got != want {
			t.Errorf("%q: %q %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "+", "abc", "1234", "+1234567890123456", "90 12 34 5x"} {
		if _, err := NormalizeNumber(raw, "228"); err == nil {
			t.Errorf("%q accepted", raw)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	bad := map[string]func(*Config){
		"no line":         func(c *Config) { c.Lines = nil },
		"line twice":      func(c *Config) { c.Lines = []Line{{ID: "a"}, {ID: "a"}} },
		"line without id": func(c *Config) { c.Lines = []Line{{}} },
		"bad country":     func(c *Config) { c.Country = "+228" },
		"bad number":      func(c *Config) { c.Contacts = map[string]string{"x": "call me"} },
		"empty contact":   func(c *Config) { c.Contacts = map[string]string{"": "90123456"} },
		"bad test number": func(c *Config) { c.TestNumber = "x" },
		"no ring":         func(c *Config) { c.RingFor = 0 },
		"retry inverted":  func(c *Config) { c.RetryMax = time.Second },
		"no restart":      func(c *Config) { c.MissesToRestart = 0 },
		"missing max age": func(c *Config) { delete(c.MaxAge, PriorityOther) },
	}
	for name, mutate := range bad {
		cfg := DefaultConfig([]Line{{ID: "line1"}}, map[string]string{"owner": "90123456"})
		cfg.MaxAge = map[Priority]time.Duration{PriorityLifeSafety: time.Minute, PriorityIntrusion: time.Minute, PriorityOther: time.Minute}
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
