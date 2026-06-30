package symphony

import (
	"errors"
	"strings"
	"testing"
)

func TestRenderPromptStrictLiquidSubset(t *testing.T) {
	desc := "body"
	attempt := 2
	prompt, err := RenderPrompt(`Issue {{ issue.identifier }} {{ issue.title }}
{% if attempt == 2 %}retry{% else %}first{% endif %}
Labels:{% for label in issue.labels %} {{ label }}{% endfor %}
Description: {{ issue.description | default: "none" }}`, Issue{
		ID:          "1",
		Identifier:  "#1",
		Title:       "Fix",
		Description: &desc,
		State:       "Todo",
		Labels:      []string{"ready", "go"},
	}, &attempt)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Issue #1 Fix", "retry", "Labels: ready go", "Description: body"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestRenderPromptUnknownVariableFails(t *testing.T) {
	_, err := RenderPrompt(`{{ issue.nope }}`, Issue{ID: "1", Identifier: "#1", Title: "Fix", State: "Todo"}, nil)
	if !errors.Is(err, ErrTemplateRender) {
		t.Fatalf("expected template render error, got %v", err)
	}
}

func TestRenderPromptUnknownFilterFails(t *testing.T) {
	_, err := RenderPrompt(`{{ issue.identifier | made_up }}`, Issue{ID: "1", Identifier: "#1", Title: "Fix", State: "Todo"}, nil)
	if !errors.Is(err, ErrTemplateRender) {
		t.Fatalf("expected template render error, got %v", err)
	}
}
