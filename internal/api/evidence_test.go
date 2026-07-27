package api

import (
	"strings"
	"testing"

	"github.com/devganeshg/kubeaura/internal/evidence"
)

// The audit line is the durable half of the disclosure: it outlives the answer
// and must identify the inputs without containing any of them.
func TestEvidenceDetail(t *testing.T) {
	hash := "9f2c1a4b7e08d3c5a6b1f0e2d3c4b5a69f2c1a4b7e08d3c5a6b1f0e2d3c4b5a6"
	cases := []struct {
		name string
		env  evidence.Envelope
		want []string
	}{
		{
			name: "hosted backend",
			env:  evidence.Envelope{Hash: hash, Bytes: 4096, Provider: "Anthropic"},
			want: []string{"evidence 9f2c1a4b7e08", "4096 bytes", "Anthropic"},
		},
		{
			name: "local backend is called out",
			env:  evidence.Envelope{Hash: hash, Bytes: 512, Provider: "Ollama (local)", Local: true},
			want: []string{"on this machine"},
		},
		{
			name: "no backend configured",
			env:  evidence.Envelope{Hash: hash},
			want: []string{"unknown backend"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evidenceDetail(tc.env)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("detail %q missing %q", got, w)
				}
			}
			if strings.Contains(got, hash) {
				t.Errorf("the audit line should carry the short hash, not the full one: %q", got)
			}
		})
	}
}

// describeDestination must not panic when the assistant is absent — the preview
// is still worth showing, it just cannot name a backend.
func TestDescribeDestinationWithoutAssistant(t *testing.T) {
	s := &Server{}
	env := evidence.Envelope{Hash: "abc"}
	s.describeDestination(&env)
	if env.Provider != "" || env.Local {
		t.Errorf("want an unstamped envelope, got %+v", env)
	}
}
