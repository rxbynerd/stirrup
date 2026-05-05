package transport

import (
	"testing"

	"google.golang.org/protobuf/proto"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
)

// TestRunConfigFromProto_SessionNamePropagates ensures that a SessionName set
// on the proto RunConfig (the wire format used by the gRPC / K8s job path) is
// copied into the internal types.RunConfig. Without this, every job dispatched
// via the control plane would silently lose its session label.
func TestRunConfigFromProto_SessionNamePropagates(t *testing.T) {
	name := "nightly-eval"
	pc := &pb.RunConfig{
		SessionName: &name,
	}

	rc := runConfigFromProto(pc)

	if rc.SessionName != "nightly-eval" {
		t.Fatalf("SessionName not propagated: got %q, want %q", rc.SessionName, "nightly-eval")
	}
}

// TestRuleOfTwoProto_EmptyMessageDoesNotDisableEnforcement guards the
// proto3 wire default for RuleOfTwoConfig.enforce. The field is declared
// `optional bool` so an unset value is wire-distinguishable from an
// explicit `false`. If a future change drops the `optional` modifier,
// any control plane that includes a RuleOfTwoConfig sub-message at all
// (even an empty one) would silently bypass Rule-of-Two enforcement —
// the opposite of the intended secure default.
func TestRuleOfTwoProto_EmptyMessageDoesNotDisableEnforcement(t *testing.T) {
	t.Run("empty message round-trips as nil enforce", func(t *testing.T) {
		original := &pb.RuleOfTwoConfig{}
		bytes, err := proto.Marshal(original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var decoded pb.RuleOfTwoConfig
		if err := proto.Unmarshal(bytes, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if decoded.Enforce != nil {
			t.Fatalf("expected Enforce nil for empty message, got %v", *decoded.Enforce)
		}

		// Confirm runConfigFromProto preserves the nil pointer so the
		// validator's secure default (enforce) applies.
		rc := runConfigFromProto(&pb.RunConfig{RuleOfTwo: &decoded})
		if rc.RuleOfTwo == nil {
			t.Fatal("expected types.RunConfig.RuleOfTwo to be non-nil when proto sub-message is present")
		}
		if rc.RuleOfTwo.Enforce != nil {
			t.Fatalf("expected types Enforce nil, got %v", *rc.RuleOfTwo.Enforce)
		}
	})

	t.Run("explicit false round-trips as &false", func(t *testing.T) {
		f := false
		original := &pb.RuleOfTwoConfig{Enforce: &f}
		bytes, err := proto.Marshal(original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var decoded pb.RuleOfTwoConfig
		if err := proto.Unmarshal(bytes, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if decoded.Enforce == nil {
			t.Fatal("expected Enforce non-nil after explicit false")
		}
		if *decoded.Enforce != false {
			t.Fatalf("expected Enforce false, got %v", *decoded.Enforce)
		}
	})

	t.Run("explicit true round-trips as &true", func(t *testing.T) {
		tr := true
		original := &pb.RuleOfTwoConfig{Enforce: &tr}
		bytes, err := proto.Marshal(original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var decoded pb.RuleOfTwoConfig
		if err := proto.Unmarshal(bytes, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if decoded.Enforce == nil {
			t.Fatal("expected Enforce non-nil after explicit true")
		}
		if *decoded.Enforce != true {
			t.Fatalf("expected Enforce true, got %v", *decoded.Enforce)
		}
	})
}

// TestRunConfigFromProto_TranslatesNewSafetyFields confirms that proto
// fields added in #42 (executor.runtime, ruleOfTwo, codeScanner,
// permissionPolicy.policy_file/fallback) are populated on the internal
// RunConfig. Without this, a control plane that sets these fields gets
// no visible behaviour change because the translate layer drops them.
func TestRunConfigFromProto_TranslatesNewSafetyFields(t *testing.T) {
	enforceFalse := false
	pc := &pb.RunConfig{
		Executor: &pb.ExecutorConfig{
			Type:    "container",
			Runtime: "runsc",
		},
		PermissionPolicy: &pb.PermissionPolicyConfig{
			Type:       "policy-engine",
			PolicyFile: "/etc/stirrup/policy.cedar",
			Fallback:   "deny-side-effects",
		},
		RuleOfTwo: &pb.RuleOfTwoConfig{Enforce: &enforceFalse},
		CodeScanner: &pb.CodeScannerConfig{
			Type:        "composite",
			Scanners:    []string{"patterns", "semgrep"},
			BlockOnWarn: true,
		},
	}

	rc := runConfigFromProto(pc)

	if rc.Executor.Runtime != "runsc" {
		t.Errorf("Executor.Runtime: got %q, want runsc", rc.Executor.Runtime)
	}
	if rc.PermissionPolicy.PolicyFile != "/etc/stirrup/policy.cedar" {
		t.Errorf("PolicyFile: got %q", rc.PermissionPolicy.PolicyFile)
	}
	if rc.PermissionPolicy.Fallback != "deny-side-effects" {
		t.Errorf("Fallback: got %q", rc.PermissionPolicy.Fallback)
	}
	if rc.RuleOfTwo == nil || rc.RuleOfTwo.Enforce == nil || *rc.RuleOfTwo.Enforce != false {
		t.Errorf("RuleOfTwo.Enforce not preserved: %+v", rc.RuleOfTwo)
	}
	if rc.CodeScanner == nil || rc.CodeScanner.Type != "composite" {
		t.Errorf("CodeScanner not translated: %+v", rc.CodeScanner)
	}
	if rc.CodeScanner != nil && len(rc.CodeScanner.Scanners) != 2 {
		t.Errorf("CodeScanner.Scanners: got %v", rc.CodeScanner.Scanners)
	}
	if rc.CodeScanner != nil && !rc.CodeScanner.BlockOnWarn {
		t.Errorf("CodeScanner.BlockOnWarn not translated")
	}
}

// TestRunConfigFromProto_SessionNameAbsentWhenNil documents the safe default:
// when the proto omits SessionName (the field is a proto3 optional, so the
// zero value is a nil pointer), the internal RunConfig must surface the empty
// string rather than panicking on a nil dereference.
func TestRunConfigFromProto_SessionNameAbsentWhenNil(t *testing.T) {
	pc := &pb.RunConfig{}

	rc := runConfigFromProto(pc)

	if rc.SessionName != "" {
		t.Fatalf("SessionName should be empty when proto field is nil, got %q", rc.SessionName)
	}

	// Cross-check: when the proto sub-message is omitted entirely,
	// types fields stay nil so ValidateRunConfig applies its defaults.
	rcEmpty := runConfigFromProto(&pb.RunConfig{})
	if rcEmpty.RuleOfTwo != nil {
		t.Error("RuleOfTwo should be nil when proto field absent")
	}
	if rcEmpty.CodeScanner != nil {
		t.Error("CodeScanner should be nil when proto field absent")
	}

	// Validator-level coupling: an unset proto Enforce must be treated
	// as "enforce" (the secure default). Add a separate types-level
	// assertion to defend against accidental sentinel-pointer rewrites.
	pcUnset := &pb.RunConfig{RuleOfTwo: &pb.RuleOfTwoConfig{}}
	rcUnset := runConfigFromProto(pcUnset)
	if rcUnset.RuleOfTwo == nil {
		t.Fatal("RuleOfTwo non-nil expected when proto field present")
	}
	if rcUnset.RuleOfTwo.Enforce != nil {
		t.Errorf("RuleOfTwo.Enforce should be nil for empty proto sub-message; got %v", *rcUnset.RuleOfTwo.Enforce)
	}
}

// TestRunConfigProtoRoundTrip_GuardRail asserts that every field on
// the proto GuardRailConfig is preserved by runConfigFromProto. A
// missing translation block for guard_rail (proto field 27) silently
// drops guard configuration on the K8s job path, leaving operators
// running a guardless harness even when their wire payload says
// otherwise. The composite stage exercises the recursion path so
// nested configurations also survive.
func TestRunConfigProtoRoundTrip_GuardRail(t *testing.T) {
	think := true
	pc := &pb.RunConfig{
		GuardRail: &pb.GuardRailConfig{
			Type:          "composite",
			Phases:        []string{"pre_turn", "post_turn"},
			Endpoint:      "",
			Model:         "",
			TimeoutMs:     1500,
			FailOpen:      true,
			MinChunkChars: 256,
			Stages: []*pb.GuardRailConfig{
				{
					Type:      "granite-guardian",
					Endpoint:  "http://classifier.local:9999",
					Model:     "ibm-granite/granite-guardian-4.1-8b",
					Threshold: 0,
					Criteria:  []string{"harm", "jailbreak"},
					CustomCriteria: map[string]string{
						"my_rule": "no profanity",
					},
					Think:         &think,
					TimeoutMs:     1500,
					FailOpen:      false,
					MinChunkChars: 256,
				},
				{
					Type:      "cloud-judge",
					Model:     "claude-haiku-4-5-20251001",
					TimeoutMs: 5000,
				},
			},
		},
	}

	rc := runConfigFromProto(pc)

	if rc.GuardRail == nil {
		t.Fatal("GuardRail dropped during proto translation")
	}
	if rc.GuardRail.Type != "composite" {
		t.Errorf("Type: got %q, want composite", rc.GuardRail.Type)
	}
	if got, want := rc.GuardRail.Phases, []string{"pre_turn", "post_turn"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Phases: got %v, want %v", got, want)
	}
	if rc.GuardRail.TimeoutMs != 1500 {
		t.Errorf("TimeoutMs: got %d, want 1500", rc.GuardRail.TimeoutMs)
	}
	if !rc.GuardRail.FailOpen {
		t.Errorf("FailOpen: got false, want true")
	}
	if rc.GuardRail.MinChunkChars != 256 {
		t.Errorf("MinChunkChars: got %d, want 256", rc.GuardRail.MinChunkChars)
	}
	if len(rc.GuardRail.Stages) != 2 {
		t.Fatalf("Stages: got %d, want 2", len(rc.GuardRail.Stages))
	}

	gg := rc.GuardRail.Stages[0]
	if gg.Type != "granite-guardian" {
		t.Errorf("Stages[0].Type: got %q", gg.Type)
	}
	if gg.Endpoint != "http://classifier.local:9999" {
		t.Errorf("Stages[0].Endpoint: got %q", gg.Endpoint)
	}
	if gg.Model != "ibm-granite/granite-guardian-4.1-8b" {
		t.Errorf("Stages[0].Model: got %q", gg.Model)
	}
	if len(gg.Criteria) != 2 || gg.Criteria[0] != "harm" || gg.Criteria[1] != "jailbreak" {
		t.Errorf("Stages[0].Criteria: got %v", gg.Criteria)
	}
	if gg.CustomCriteria["my_rule"] != "no profanity" {
		t.Errorf("Stages[0].CustomCriteria: got %v", gg.CustomCriteria)
	}
	if gg.Think == nil || *gg.Think != true {
		t.Errorf("Stages[0].Think: got %v, want pointer to true", gg.Think)
	}

	cj := rc.GuardRail.Stages[1]
	if cj.Type != "cloud-judge" {
		t.Errorf("Stages[1].Type: got %q", cj.Type)
	}
	if cj.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("Stages[1].Model: got %q", cj.Model)
	}
	if cj.TimeoutMs != 5000 {
		t.Errorf("Stages[1].TimeoutMs: got %d", cj.TimeoutMs)
	}
}

// TestRunConfigProtoRoundTrip_GuardRailNilStays_Nil documents that an
// absent guard_rail proto field translates to a nil GuardRail (the
// validator then applies the default — guards are opt-in).
func TestRunConfigProtoRoundTrip_GuardRailNilStaysNil(t *testing.T) {
	rc := runConfigFromProto(&pb.RunConfig{})
	if rc.GuardRail != nil {
		t.Errorf("GuardRail should be nil when proto field absent; got %+v", rc.GuardRail)
	}
}
