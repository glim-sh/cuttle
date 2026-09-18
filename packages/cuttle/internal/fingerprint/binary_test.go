package fingerprint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureBinary(t *testing.T) {
	realBin := filepath.Join(t.TempDir(), "chrome")
	if err := os.WriteFile(realBin, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		value   string
		want    string
		wantErr error
	}{
		{name: "unset", value: "", wantErr: errBinaryPathUnset},
		{name: "missing", value: "/no/such/chrome", wantErr: errBinaryPathMissing},
		{name: "present", value: realBin, want: realBin},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(BinaryPathEnv, tt.value)
			got, err := EnsureBinary()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
