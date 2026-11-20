// verification_id_test.go 回归验证命令检查身份的指纹与合法性边界。
//
// Author: fxbin
package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVerificationCommandIDOmittedWhenEmpty(t *testing.T) {
	encoded, err := json.Marshal(VerificationCommand{Argv: []string{"/usr/bin/true"}})
	if err != nil {
		t.Fatalf("marshal empty ID command: %v", err)
	}
	if strings.Contains(string(encoded), `"id"`) {
		t.Fatalf("empty verification command id must be omitted from fingerprint bytes: %s", encoded)
	}
	encoded, err = json.Marshal(VerificationCommand{ID: "build", Argv: []string{"/usr/bin/true"}})
	if err != nil {
		t.Fatalf("marshal named ID command: %v", err)
	}
	if !strings.Contains(string(encoded), `"id":"build"`) {
		t.Fatalf("non-empty verification command id must enter fingerprint bytes: %s", encoded)
	}
}

func TestVerificationCommandIDValidation(t *testing.T) {
	cases := []struct {
		name    string
		ids     []string
		wantErr bool
	}{
		{"distinct valid", []string{"build", "test"}, false},
		{"duplicate", []string{"build", "build"}, true},
		{"invalid characters", []string{"bad id"}, true},
		{"empty means legacy", []string{"", ""}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			configuration := validDefaults()
			configuration.Verification.Commands = make([]VerificationCommand, 0, len(testCase.ids))
			for _, id := range testCase.ids {
				configuration.Verification.Commands = append(configuration.Verification.Commands, VerificationCommand{ID: id, Argv: []string{"/usr/bin/true"}})
			}
			err := Validate(configuration)
			if (err != nil) != testCase.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, testCase.wantErr)
			}
		})
	}
}
