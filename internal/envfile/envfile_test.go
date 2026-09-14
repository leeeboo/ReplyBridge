package envfile

import (
	"errors"
	"strings"
	"testing"
)

func TestLoadParsesWithoutExecutingOrOverriding(t *testing.T) {
	values := map[string]string{"EXISTING": "keep"}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	set := func(key, value string) error { values[key] = value; return nil }
	input := "# comment\nexport API_KEY='secret value'\nEXISTING=replace\nLITERAL=$(never-run)\n"
	if err := Load(strings.NewReader(input), lookup, set); err != nil {
		t.Fatal(err)
	}
	if values["API_KEY"] != "secret value" || values["EXISTING"] != "keep" || values["LITERAL"] != "$(never-run)" {
		t.Fatalf("unexpected values: %#v", values)
	}
}

func TestLoadRejectsInvalidLineWithoutLeakingValue(t *testing.T) {
	secret := "must-not-appear"
	err := Load(strings.NewReader("BAD KEY="+secret), func(string) (string, bool) { return "", false }, func(string, string) error { return nil })
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestLoadWrapsSetterErrorWithoutValue(t *testing.T) {
	secret := "must-not-appear"
	err := Load(strings.NewReader("KEY="+secret), func(string) (string, bool) { return "", false }, func(string, string) error { return errors.New("denied") })
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe error: %v", err)
	}
}
