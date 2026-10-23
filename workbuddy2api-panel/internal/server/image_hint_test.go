package server

import "testing"

func TestHasImagePartMixedContent(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"image only", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`, true},
		{"text system and image", `{"messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":[{"type":"text","text":"Describe this"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`, true},
		{"text history after image", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.com/a.png"}]},{"role":"assistant","content":"A picture."},{"role":"user","content":"Tell me more."}]}`, true},
		{"null tool-call content", `{"messages":[{"role":"assistant","content":null,"tool_calls":[]},{"role":"tool","content":"done"},{"role":"user","content":[{"type":"image_url","image_url":"https://example.com/a.png"}]}]}`, true},
		{"text and text parts", `{"messages":[{"role":"system","content":"Describe images"},{"role":"user","content":[{"type":"text","text":"image_url"}]}]}`, false},
		{"unrelated image key", `{"metadata":{"image_url":"https://example.com/a.png"},"messages":[{"role":"user","content":"hi"}]}`, false},
		{"missing content", `{"messages":[{"role":"assistant"}]}`, false},
		{"invalid content shape", `{"messages":[{"content":{"type":"image_url"}}]}`, false},
		{"malformed json", `{"messages":[{"content":[{"type":"image_url"}]}]`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasImagePart([]byte(tt.body)); got != tt.want {
				t.Errorf("hasImagePart() = %v, want %v", got, tt.want)
			}
		})
	}
}
