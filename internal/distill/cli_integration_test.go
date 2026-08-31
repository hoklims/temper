package distill

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/hoklims/temper/internal/llm"
	"github.com/hoklims/temper/internal/outbound"
)

func TestCodexSuggestionSchemaSmoke(t *testing.T) {
	if os.Getenv("TEMPER_CODEX_SCHEMA_SMOKE") == "" {
		t.Skip("set TEMPER_CODEX_SCHEMA_SMOKE=1 to validate the distillation schema with Codex")
	}
	provider, err := llm.NewCodex("")
	if err != nil {
		t.Fatal(err)
	}
	var builder outbound.Builder
	builder.Static(`Return exactly {"suggestions":[]}.`)
	payload, err := builder.BuildWithOutputSchema("Return only the requested JSON object.", suggestionOutputSchema)
	if err != nil {
		t.Fatal(err)
	}
	output, err := provider.Generate(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	var result rawResponse
	if json.Unmarshal([]byte(output), &result) != nil || result.Suggestions == nil || len(result.Suggestions) != 0 {
		t.Fatalf("output = %s", output)
	}
}
