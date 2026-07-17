package mistral

import (
	"strings"

	providerUtils "github.com/grevinden/bifrost/core/providers/utils"
	"github.com/grevinden/bifrost/core/schemas"
)

func (response *MistralListModelsResponse) ToBifrostListModelsResponse(allowedModels schemas.WhiteList, blacklistedModels schemas.BlackList, aliases schemas.KeyAliases, unfiltered bool) *schemas.BifrostListModelsResponse {
	if response == nil {
		return nil
	}

	bifrostResponse := &schemas.BifrostListModelsResponse{
		Data: make([]schemas.Model, 0, len(response.Data)),
	}

	pipeline := &providerUtils.ListModelsPipeline{
		AllowedModels:     allowedModels,
		BlacklistedModels: blacklistedModels,
		Aliases:           aliases,
		Unfiltered:        unfiltered,
		ProviderKey:       schemas.Mistral,
		MatchFns:          providerUtils.DefaultMatchFns(),
	}
	if pipeline.ShouldEarlyExit() {
		return bifrostResponse
	}

	included := make(map[string]bool)

	for _, model := range response.Data {
		for _, result := range pipeline.FilterModel(model.ID) {
			entry := schemas.Model{
				ID:            string(schemas.Mistral) + "/" + result.ResolvedID,
				Name:          new(model.Name),
				Description:   new(model.Description),
				Created:       new(model.Created),
				ContextLength: new(int(model.MaxContextLength)),
				OwnedBy:       new(model.OwnedBy),
			}
			if result.AliasValue != "" {
				entry.Alias = new(result.AliasValue)
			}
			bifrostResponse.Data = append(bifrostResponse.Data, entry)
			included[strings.ToLower(result.ResolvedID)] = true
		}
	}

	bifrostResponse.Data = append(bifrostResponse.Data,
		pipeline.BackfillModels(included)...)

	return bifrostResponse
}
