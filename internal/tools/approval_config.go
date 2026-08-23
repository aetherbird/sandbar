package tools

import (
	"fmt"

	"sandbar/internal/config"
)

// ApprovalConfigFromToolConfig converts the serializable runtime config into
// the policy engine's typed form. Callers may attach an argument-aware Resolver
// to the returned value before installing it on a Registry.
func ApprovalConfigFromToolConfig(src config.ToolApprovalConfig) (ApprovalConfig, error) {
	if err := src.Validate(); err != nil {
		return ApprovalConfig{}, err
	}
	cfg := ApprovalConfig{
		Mode:         ApprovalMode(src.Mode),
		ToolPolicies: make(map[string]ApprovalPolicy, len(src.Rules)),
		TierPolicies: make(map[AccessTier]ApprovalPolicy, len(src.Tiers)),
	}
	if cfg.Mode == "" {
		cfg.Mode = ApprovalModeYolo
	}
	for name, policy := range src.Rules {
		cfg.ToolPolicies[name] = ApprovalPolicy(policy)
	}
	for tier, policy := range src.Tiers {
		cfg.TierPolicies[AccessTier(tier)] = ApprovalPolicy(policy)
	}
	if err := cfg.Validate(); err != nil {
		return ApprovalConfig{}, fmt.Errorf("convert tool approval config: %w", err)
	}
	return cfg, nil
}
