package scopes

import (
	"slices"
	"testing"
)

func TestForMode(t *testing.T) {
	tests := []struct {
		readOnly, destructive, settings bool
		want                            []string
	}{
		{true, false, false, []string{Readonly}},
		{false, false, false, []string{Modify}},
		{false, true, false, []string{Full}},
		{true, true, false, []string{Full}},
		{false, false, true, []string{Modify, SettingsBasic}},
		{false, true, true, []string{Full, SettingsBasic}},
	}
	for _, tt := range tests {
		if got := ForMode(tt.readOnly, tt.destructive, tt.settings); !slices.Equal(got, tt.want) {
			t.Errorf("ForMode(%t, %t, %t) = %v, want %v", tt.readOnly, tt.destructive, tt.settings, got, tt.want)
		}
	}
}

// The send flag must not change the scope set: §2.10, the default scope
// already sends.
func TestModesSendChangesNothing(t *testing.T) {
	modes := Modes()
	if len(modes) != 5 {
		t.Fatalf("got %d modes, want 5", len(modes))
	}
	byName := map[string]Mode{}
	for _, m := range modes {
		byName[m.Name] = m
		if len(m.Scopes) == 0 {
			t.Errorf("mode %s requests nothing", m.Name)
		}
	}
	if !slices.Equal(byName["send"].Scopes, byName["default"].Scopes) {
		t.Errorf("send mode scopes %v differ from default %v", byName["send"].Scopes, byName["default"].Scopes)
	}
	if !slices.Equal(byName["destructive"].Scopes, []string{Full}) {
		t.Errorf("destructive mode requests %v", byName["destructive"].Scopes)
	}
	// Only destructive asks for the full scope (§4.6).
	for _, m := range modes {
		if m.Name != "destructive" && slices.Contains(m.Scopes, Full) {
			t.Errorf("mode %s requests %s", m.Name, Full)
		}
	}
}

func TestSatisfied(t *testing.T) {
	tests := []struct {
		required string
		granted  []string
		want     bool
	}{
		{Readonly, []string{Readonly}, true},
		{Readonly, []string{Modify}, true},
		{Readonly, []string{Full}, true},
		{Modify, []string{Full}, true},
		{Compose, []string{Modify}, true},
		{Labels, []string{Full}, true},
		{Modify, []string{Readonly}, false},
		{Full, []string{Modify}, false},
		{Readonly, []string{Compose}, false},
		{Readonly, nil, false},
	}
	for _, tt := range tests {
		if got := Satisfied(tt.required, tt.granted); got != tt.want {
			t.Errorf("Satisfied(%s, %v) = %t, want %t", tt.required, tt.granted, got, tt.want)
		}
	}
}

func TestMissingAndCovered(t *testing.T) {
	if got := Missing([]string{Readonly}, []string{Modify, Readonly}); !slices.Equal(got, []string{Modify}) {
		t.Errorf("Missing = %v", got)
	}
	if !Covered([]string{Full}, ForMode(false, false, false)) {
		t.Error("full scope does not cover the default set")
	}
	if Covered([]string{Modify}, ForMode(false, true, false)) {
		t.Error("modify covers the destructive set; a destructive login would skip consent")
	}
	if Covered(nil, ForMode(true, false, false)) {
		t.Error("nothing granted covers something")
	}
	// No other scope covers the settings writes, not even the full one.
	for _, destructive := range []bool{false, true} {
		set := ForMode(false, destructive, true)
		if !slices.Contains(set, SettingsBasic) || Covered(ForMode(false, destructive, false), set) {
			t.Errorf("destructive=%v: settings set %v, or covered without gmail.settings.basic", destructive, set)
		}
	}
	if Satisfied(SettingsBasic, []string{Full}) {
		t.Error("the full scope satisfies gmail.settings.basic; Google says it does not")
	}
}

func TestExcess(t *testing.T) {
	for _, tc := range []struct {
		granted, required, want []string
	}{
		{[]string{Full}, []string{Full}, nil},
		{[]string{Full}, []string{Modify}, []string{Full}},
		{[]string{Modify}, []string{Readonly}, []string{Modify}},
		{[]string{Modify, "openid", "email"}, []string{Modify}, nil},
		{[]string{Readonly}, []string{Modify}, nil},
		{[]string{Readonly, Labels}, []string{Modify}, nil},
	} {
		if got := Excess(tc.granted, tc.required); !slices.Equal(got, tc.want) {
			t.Errorf("Excess(%v, %v) = %v; want %v", tc.granted, tc.required, got, tc.want)
		}
	}
}
