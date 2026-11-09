package server

import (
	"testing"

	"wen/internal/llm"
)

func TestInheritTestProtocol(t *testing.T) {
	cases := []struct {
		name      string
		reqType   string
		savedType string
		requested string
		saved     string
		want      string
	}{
		{"explicit value wins", llm.TypeOpenAICompat, llm.TypeOpenAICompat, llm.ProtocolChat, llm.ProtocolResponses, llm.ProtocolChat},
		{"same openai provider inherits", llm.TypeOpenAICompat, llm.TypeOpenAICompat, "", llm.ProtocolResponses, llm.ProtocolResponses},
		{"switched to anthropic does not inherit", llm.TypeAnthropic, llm.TypeOpenAICompat, "", llm.ProtocolResponses, ""},
		{"saved anthropic is not inherited", llm.TypeOpenAICompat, llm.TypeAnthropic, "", llm.ProtocolResponses, ""},
		{"empty saved stays empty", llm.TypeOpenAICompat, llm.TypeOpenAICompat, "", "", ""},
	}
	for _, c := range cases {
		if got := inheritTestProtocol(c.reqType, c.savedType, c.requested, c.saved); got != c.want {
			t.Errorf("%s: inheritTestProtocol(%q,%q,%q,%q) = %q, want %q",
				c.name, c.reqType, c.savedType, c.requested, c.saved, got, c.want)
		}
	}
}
