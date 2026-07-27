package ai

import "testing"

func withProvider(p Provider) *Assistant {
	return &Assistant{
		providers: map[string]Provider{"c": p},
		active:    "c",
	}
}

// OnMachine is what the evidence envelope shows an operator to answer "does
// this leave my laptop". Getting it wrong in the permissive direction is the
// failure that matters, so a non-loopback host must never read as local.
func TestOnMachine(t *testing.T) {
	cases := []struct {
		name string
		a    *Assistant
		want bool
	}{
		{"no backend", &Assistant{}, false},
		{"ollama default", withProvider(newOllama("", "llama3.2")), true},
		{"ollama on 127.0.0.1", withProvider(newOllama("http://127.0.0.1:11434", "m")), true},
		{"ollama on the LAN is not this machine", withProvider(newOllama("http://192.168.1.20:11434", "m")), false},
		{"lm studio on localhost", withProvider(newOpenAI("http://localhost:1234/v1", "", "m", "LM Studio")), true},
		{"openai itself", withProvider(newOpenAI("", "k", "gpt-4o-mini", "")), false},
		{"anthropic", withProvider(newAnthropic("k", "")), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.OnMachine(); got != tc.want {
				t.Errorf("OnMachine() = %v, want %v (endpoint %q)", got, tc.want, tc.a.Endpoint())
			}
		})
	}
}
