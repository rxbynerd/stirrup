package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"

	"github.com/rxbynerd/stirrup/types"
)

// applyCorrelationOverrides folds the --correlation-* flags into cfg. Each
// scalar flag replaces only its own field, an explicitly empty value
// clearing it, and labels merge key by key over any file-supplied labels.
// The block is left nil when no flag is set, and is reset to nil when
// flags empty a file-supplied block. Key shape and size limits are left to
// ValidateRunConfig.
func applyCorrelationOverrides(cmd *cobra.Command, cfg *types.RunConfig) error {
	f := cmd.Flags()
	scalars := []struct {
		flag  string
		field func(*types.CorrelationConfig) *string
	}{
		{"correlation-session-id", func(c *types.CorrelationConfig) *string { return &c.SessionID }},
		{"correlation-task-id", func(c *types.CorrelationConfig) *string { return &c.TaskID }},
		{"correlation-attempt-id", func(c *types.CorrelationConfig) *string { return &c.AttemptID }},
		{"correlation-user-id", func(c *types.CorrelationConfig) *string { return &c.UserID }},
		{"correlation-url", func(c *types.CorrelationConfig) *string { return &c.URL }},
	}

	anyChanged := f.Changed("correlation-label")
	for _, s := range scalars {
		anyChanged = anyChanged || f.Changed(s.flag)
	}
	if !anyChanged {
		return nil
	}

	var labels map[string]string
	if f.Changed("correlation-label") {
		// GetStringArray round-trips through a CSV string and drops an
		// empty entry; the slice value keeps it so it is rejected below.
		slice, ok := f.Lookup("correlation-label").Value.(flag.SliceValue)
		if !ok {
			return fmt.Errorf("--correlation-label: flag is not a repeatable value")
		}
		entries := slice.GetSlice()
		for _, entry := range entries {
			k, v, err := parseQueryParam(entry)
			if err != nil {
				return fmt.Errorf("--correlation-label %q: %w", entry, err)
			}
			if v == "" {
				return fmt.Errorf("--correlation-label %q: empty value (entry: %q)", entry, entry)
			}
			if labels == nil {
				labels = map[string]string{}
			}
			labels[k] = v
		}
	}

	if cfg.Correlation == nil {
		cfg.Correlation = &types.CorrelationConfig{}
	}
	corr := cfg.Correlation
	for _, s := range scalars {
		if f.Changed(s.flag) {
			*s.field(corr), _ = f.GetString(s.flag)
		}
	}
	for k, v := range labels {
		if corr.Labels == nil {
			corr.Labels = map[string]string{}
		}
		corr.Labels[k] = v
	}
	if corr.SessionID == "" && corr.TaskID == "" && corr.AttemptID == "" && corr.UserID == "" && corr.URL == "" && len(corr.Labels) == 0 {
		cfg.Correlation = nil
	}
	return nil
}
