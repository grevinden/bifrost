package huggingface

import (
	"testing"

	providerUtils "github.com/grevinden/bifrost/core/providers/utils"
	schemas "github.com/grevinden/bifrost/core/schemas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPayloadOrdering_HuggingFaceChatRequest(t *testing.T) {
	req := &HuggingFaceChatRequest{
		Model: "meta-llama/Llama-3-70B-Instruct",
		Messages: []schemas.ChatMessage{
			{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: new("hello")}},
		},
		Temperature: new(0.7),
		Stream:      new(true),
		Tools: []schemas.ChatTool{
			{
				Type: "function",
				Function: &schemas.ChatToolFunction{
					Name:        "get_weather",
					Description: new("Get weather"),
					Parameters: &schemas.ToolFunctionParameters{
						Type: "object",
						Properties: schemas.NewOrderedMapFromPairs(
							schemas.KV("location", map[string]any{"type": "string"}),
						),
						Required: []string{"location"},
					},
				},
			},
		},
	}

	result, err := providerUtils.MarshalSorted(req)
	require.NoError(t, err)

	golden := `{"messages":[{"role":"user","content":"hello"}],"model":"meta-llama/Llama-3-70B-Instruct","stream":true,"temperature":0.7,"tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}}}]}`

	assert.Equal(t, golden, string(result), "payload field ordering changed — if intentional, update the golden string")

	// Determinism: 100 iterations must produce identical bytes
	for i := range 100 {
		iter, err := providerUtils.MarshalSorted(req)
		require.NoError(t, err)
		assert.Equal(t, string(result), string(iter), "non-deterministic marshal output on iteration %d", i)
	}
}
