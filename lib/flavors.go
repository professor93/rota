package rota

import "strings"

// Which CLI understands which request field.
//
// This is a rule, not a description: a field the chosen CLI cannot honour is
// refused by name rather than dropped, and that refusal is the library's
// business. What a field is called in a form, what it means, and what an
// example looks like are a transport's business — a transport should ask
// FlavorsOf, so there is only ever one list.

// fieldFlavors maps a request field to the CLI vocabularies that understand
// it. A field absent from this table is understood by all of them.
//
// It is written by hand and the argv builders are what actually know, so
// flavors_test.go ties the two together rather than trusting them to agree:
// every name here must be a real Spec field, every CLI this says does not
// understand a field must refuse it by name, and a CLI this says does
// understand a permission-shaped field must build a different command line
// with it than without. A stale entry is not a documentation bug — it is a
// request accepted and ignored, which is the thing this table exists to
// prevent.
var fieldFlavors = map[string][]string{
	"json_schema":                            {"claude", "codex", "grok"},
	"fallback_model":                         {"claude"},
	"max_turns":                              {"grok"},
	"rules":                                  {"grok"},
	"verbatim":                               {"grok"},
	"max_budget_usd":                         {"claude"},
	"session_id":                             {"claude", "grok"},
	"fork_session":                           {"claude", "codex", "grok"},
	"ephemeral":                              {"claude", "codex"},
	"hermetic":                               {"claude"},
	"name":                                   {"claude"},
	"system_prompt":                          {"claude", "grok"},
	"append_system_prompt":                   {"claude"},
	"setting_sources":                        {"claude"},
	"settings":                               {"claude"},
	"agents":                                 {"claude", "grok"},
	"agent":                                  {"claude", "grok", "kimi"},
	"mcp_config":                             {"claude"},
	"strict_mcp_config":                      {"claude"},
	"plugin_dirs":                            {"claude"},
	"plugin_urls":                            {"claude"},
	"autocompact":                            {"claude"},
	"exclude_dynamic_system_prompt_sections": {"claude"},
	"worktree":                               {"claude", "grok"},
	"images":                                 {"codex"},
	"profile":                                {"codex"},
	"config":                                 {"codex"},
	"enable":                                 {"codex"},
	"disable":                                {"codex"},
	"strict_config":                          {"codex"},
	"skip_git_repo_check":                    {"codex"},
	"ignore_user_config":                     {"codex"},
	"ignore_rules":                           {"codex"},
	"permission_mode":                        {"kimi", "grok", "claude"},
	"sandbox":                                {"grok", "codex"},
	"always_approve":                         {"grok"},
	"no_plan":                                {"grok"},
	"no_subagents":                           {"grok"},
	"disable_web_search":                     {"grok"},
	"restricted":                             {"claude"},
	"allowed_tools":                          {"claude", "grok"},
	"disallowed_tools":                       {"claude", "grok"},
	"tools":                                  {"claude", "grok"},
	"approve_for_me":                         {"codex"},
	"safe_mode":                              {"claude"},
	"disable_slash_commands":                 {"claude"},
	"dangerously_skip_permissions":           {"claude"},
	"allow_dangerously_skip_permissions":     {"claude"},
	"dangerously_bypass_approvals_and_sandbox": {"codex"},
	"dangerously_bypass_hook_trust":            {"codex"},
	"include_partial_messages":                 {"claude", "grok"},
	"include_hook_events":                      {"claude"},
	"forward_subagent_text":                    {"claude"},
	"prompt_suggestions":                       {"claude"},
	"verbose":                                  {"claude"},
	"debug":                                    {"claude", "grok"},
}

// FlavorsOf reports the CLI vocabularies that understand a request field.
// Nil means every one of them, which is also what an unknown name returns:
// a field this package does not restrict is not this package's to refuse.
func FlavorsOf(field string) []string {
	f, ok := fieldFlavors[field]
	if !ok {
		return nil
	}
	return append([]string(nil), f...)
}

// RestrictedFields names every field this package refuses to some flavor. It
// exists so a description of the request vocabulary can be checked against
// the rules rather than drifting away from them.
func RestrictedFields() []string {
	out := make([]string, 0, len(fieldFlavors))
	for name := range fieldFlavors {
		out = append(out, name)
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// checkFlavor refuses a field the chosen CLI cannot honour.
//
// Dropping it instead would be worse than useless: a caller asking for
// `sandbox: read-only` on a claude account would get an unsandboxed run and
// a success, having been told nothing. Silence about a safety option is the
// one failure mode worth being noisy about.
func checkFlavor(provider, flavor string, set []string) error {
	for _, name := range set {
		flavors, known := fieldFlavors[name]
		if !known || len(flavors) == 0 {
			continue
		}
		if !containsString(flavors, flavor) {
			return failf(ErrInvalidRequest,
				"%s does not understand %q; that field belongs to: %s",
				provider, name, strings.Join(flavors, ", "))
		}
	}
	return nil
}
