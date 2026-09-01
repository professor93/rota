package wire

import (
	"slices"

	rota "github.com/professor93/rota/lib"
)

// Field describes one Spec field: enough for a form to render it and for a
// person to understand it without reading a vendor's manual.
//
// It is a description, not a rule. Which CLI understands a field is decided
// in lib and asked for here, so there is only ever one list; what the field
// is called, which group it belongs in and what an example looks like are a
// transport's business, which is why they live in this package.
type Field struct {
	Name string `json:"name"`
	// Kind is string, text, bool, number, list, json, map, enum or files. "text" is
	// a string that wants several lines.
	Kind string `json:"kind"`
	// Group is a UI grouping: core, session, context, permissions, stream.
	Group string `json:"group"`
	// Label is a short human name; Help is one sentence on what it does and
	// when it matters.
	Label string `json:"label"`
	Help  string `json:"help,omitempty"`
	// Primary marks the handful of fields most requests actually set, so a
	// form can lead with them.
	Primary bool `json:"primary,omitzero"`
	// Enum lists the accepted values when Kind is enum. It comes from the
	// provider's own catalog rather than from this table: what a CLI accepts
	// is lib's to say.
	Enum []string `json:"enum,omitempty"`
	// Default is what rota sends when the field is left out, where that is
	// known and worth showing.
	Default string `json:"default,omitempty"`
	// Placeholder is an example value, shown greyed in an empty input.
	Placeholder string `json:"placeholder,omitempty"`
	// Dangerous marks a field, or specific values of it, that a caller must
	// explicitly allow (rota.Limits.AllowDangerous).
	Dangerous []string `json:"dangerous,omitempty"`
	// Flavors names the CLI vocabularies that understand this field, filled
	// in from lib. Empty means every one.
	Flavors []string `json:"flavors,omitempty"`
}

var specFields = []Field{
	// ---------------------------------------------------------------- core
	{Name: "prompt", Kind: "text", Group: "core", Label: "Prompt", Primary: true,
		Placeholder: "What should the agent do?",
		Help:        "Required. Sent on the CLI's standard input for every provider but kimi, whose CLI takes it only as an argument: a kimi prompt is visible in the local process table while the run lasts."},
	{Name: "model", Kind: "enum", Group: "core", Label: "Model", Primary: true,
		Help: "Which model to use. Only models this account's own provider offers are accepted; leaving it empty uses the provider's default."},
	{Name: "effort", Kind: "enum", Group: "core", Label: "Reasoning effort", Primary: true,
		Help: "How hard the model thinks before answering. Higher costs more and takes longer; providers that have no such setting do not offer this field."},
	{Name: "stream", Kind: "bool", Group: "core", Label: "Stream events", Primary: true,
		Help: "Send each event as it happens instead of one answer at the end. Useful for long runs; the reply arrives as Server-Sent Events."},
	{Name: "json_schema", Kind: "json", Group: "core", Label: "JSON schema",
		Placeholder: `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`,
		Help:        "Force the final answer to match this JSON Schema, so it can be parsed rather than read. Claude Code and Grok Build return the validated object in structured_output; codex puts it in the final message."},
	{Name: "fallback_model", Kind: "string", Group: "core", Label: "Fallback model",
		Placeholder: "sonnet",
		Help:        "Used when the chosen model is overloaded or unavailable, so a long job does not simply fail."},
	{Name: "max_turns", Kind: "number", Group: "core", Label: "Maximum turns",
		Placeholder: "12",
		Help:        "Stop after this many agent turns, whether or not the job is finished. A ceiling for an unattended run."},
	{Name: "rules", Kind: "text", Group: "context", Label: "Extra rules",
		Help: "Extra instructions appended to the agent's system prompt, without replacing it."},
	{Name: "verbatim", Kind: "bool", Group: "context", Label: "Send the prompt verbatim",
		Help: "Pass the prompt through exactly as written, with none of the CLI's own preprocessing."},
	{Name: "max_budget_usd", Kind: "number", Group: "core", Label: "Budget (USD)",
		Placeholder: "5.00",
		Help:        "Stop the run once it has spent this much. A hard ceiling for an unattended job."},
	{Name: "timeout_seconds", Kind: "number", Group: "core", Label: "Timeout (seconds)",
		Help: "Give up after this long and kill the CLI. Capped by the server's own limit."},
	{Name: "one_shot", Kind: "bool", Group: "core", Label: "One-shot question",
		Help: "Ask for an answer rather than work in a directory: rota then picks the defaults that make each CLI answer instead of refusing, such as a read-only sandbox."},
	{Name: "include_events", Kind: "bool", Group: "core", Label: "Return every event",
		Help: "Include the CLI's whole event stream in the reply, not just the outcome. Verbose, but it shows every tool call."},

	// ------------------------------------------------------------- session
	{Name: "session_id", Kind: "string", Group: "session", Label: "New session id",
		Placeholder: "a UUID you choose",
		Help:        "Name a brand-new conversation, so you can resume it later by this id."},
	{Name: "resume", Kind: "string", Group: "session", Label: "Resume session",
		Placeholder: "session id",
		Help:        "Continue an earlier conversation, with everything it remembers. Codex also accepts \"last\" for its most recent one."},
	{Name: "continue", Kind: "bool", Group: "session", Label: "Continue latest here",
		Help: "Resume the most recent conversation in the working directory, without having to know its id. Handy locally, ambiguous on a shared server."},
	{Name: "fork_session", Kind: "bool", Group: "session", Label: "Fork on resume",
		Help: "Branch off the resumed conversation into a new one, leaving the original untouched. Without a session to resume it does nothing."},
	{Name: "ephemeral", Kind: "bool", Group: "session", Label: "Do not save the session",
		Help: "Write no session files at all, so the run leaves nothing behind and cannot be resumed afterwards — right for a stateless request."},
	{Name: "name", Kind: "string", Group: "session", Label: "Session name",
		Help: "A label for this session, shown in the CLI's own session picker."},

	// ------------------------------------------------------------- context
	{Name: "cwd", Kind: "string", Group: "context", Label: "Working directory", Primary: true,
		Help: "Where the agent runs and which files it sees. The server may confine this to directories it was started with."},
	{Name: "files", Kind: "files", Group: "context", Label: "Files", Primary: true,
		Help: "Upload files with the request. They land in a directory private to this run, which the agent may read, and are deleted afterwards."},
	{Name: "add_dirs", Kind: "list", Group: "context", Label: "Extra directories",
		Placeholder: "/srv/data, /srv/docs",
		Help:        "Further directories the agent may read and write, beyond the working directory."},
	{Name: "scratch_dir", Kind: "string", Group: "context", Label: "Scratch directory",
		Placeholder: "/srv/scratch",
		Help:        "Where request-scoped temp files land (a grok prompt file, a codex schema). Empty means the system default; confined like every other path."},
	{Name: "hermetic", Kind: "bool", Group: "context", Label: "Hermetic",
		Help: "Run claude in a throwaway config directory: no shared identity, memory, skills or settings reach the run, and the directory is removed when it ends."},
	{Name: "system_prompt", Kind: "text", Group: "context", Label: "System prompt",
		Help: "Replace the CLI's own system prompt entirely. Powerful and easy to get wrong — usually you want the append field below."},
	{Name: "append_system_prompt", Kind: "text", Group: "context", Label: "Append to system prompt",
		Help: "Add instructions after the CLI's own system prompt, keeping its behaviour and adding yours."},
	{Name: "setting_sources", Kind: "list", Group: "context", Label: "Settings sources",
		Default: "none", Placeholder: "user, project, local",
		Help: "Which settings files on the machine to honour. Empty — the default — gives a clean session that ignores whatever happens to sit in the directory."},
	{Name: "settings", Kind: "json", Group: "context", Label: "Settings",
		Help: "A settings object applied to this run only, or a path to a settings file."},
	{Name: "agents", Kind: "json", Group: "context", Label: "Custom agents",
		Placeholder: `{"reviewer":{"description":"Reviews code","prompt":"You are a code reviewer"}}`,
		Help:        "Define subagents this run may delegate work to, each with its own instructions."},
	{Name: "agent", Kind: "string", Group: "context", Label: "Agent",
		Help: "Run as one of the named agents above, rather than as the default assistant."},
	{Name: "mcp_config", Kind: "list", Group: "context", Label: "MCP servers",
		Help: "Model Context Protocol servers to connect: a path to a config file, or the config itself as JSON."},
	{Name: "strict_mcp_config", Kind: "bool", Group: "context", Label: "Only these MCP servers",
		Help: "Ignore every MCP server configured elsewhere on this machine, using only the ones named above."},
	{Name: "plugin_dirs", Kind: "list", Group: "context", Label: "Plugin directories",
		Help: "Load plugins from these directories for this run only, without installing them."},
	{Name: "plugin_urls", Kind: "list", Group: "context", Label: "Plugin URLs",
		Help: "Fetch plugin archives from these URLs and load them for this run only. A plugin is code and its hooks are commands, so a confined server refuses this and takes plugin_dirs instead."},
	{Name: "autocompact", Kind: "string", Group: "context", Label: "Auto-compact",
		Placeholder: "auto, or 200000",
		Help:        "When to summarise the conversation to free context: \"auto\", or a token count."},
	{Name: "exclude_dynamic_system_prompt_sections", Kind: "bool", Group: "context", Label: "Portable system prompt",
		Help: "Move machine-specific details out of the system prompt, so repeated calls share a prompt cache."},
	{Name: "worktree", Kind: "string", Group: "context", Label: "Git worktree",
		Placeholder: `"true", or a name`,
		Help:        "Run inside a fresh git worktree, so the agent's edits stay off your branch."},
	{Name: "images", Kind: "list", Group: "context", Label: "Images",
		Help: "Names of files uploaded above, to attach to the prompt as images the model can see."},
	{Name: "profile", Kind: "string", Group: "context", Label: "Config profile",
		Help: "Layer a named profile from the account's own config directory over its base configuration."},
	{Name: "config", Kind: "map", Group: "context", Label: "Config overrides",
		Placeholder: "model_verbosity = high",
		Help:        "Override individual config values for this run, as key/value pairs. One of them names the endpoint the run is sent to, so a confined server refuses this unless it was started with --allow-raw-flags."},
	{Name: "enable", Kind: "list", Group: "context", Label: "Enable features",
		Help: "Turn named experimental features on for the duration of this run."},
	{Name: "disable", Kind: "list", Group: "context", Label: "Disable features",
		Help: "Turn named features off for the duration of this run."},
	{Name: "strict_config", Kind: "bool", Group: "context", Label: "Strict config",
		Help: "Fail if the config file contains anything this version does not recognise, instead of ignoring it."},
	{Name: "skip_git_repo_check", Kind: "bool", Group: "context", Label: "Allow outside a git repo",
		Help: "Run even when the working directory is not a git repository."},
	{Name: "ignore_user_config", Kind: "bool", Group: "context", Label: "Ignore user config",
		Help: "Do not read the account's own config file; credentials are still used."},
	{Name: "ignore_rules", Kind: "bool", Group: "context", Label: "Ignore rule files",
		Help: "Do not load the user or project execution-policy rules that would otherwise apply."},

	// --------------------------------------------------------- permissions
	{Name: "permission_mode", Kind: "enum", Group: "permissions", Label: "Permission mode", Primary: true,
		Dangerous: []string{"bypassPermissions"},
		Help:      "How the agent asks before acting: plan only, accept edits, or ask for nothing. Nobody is watching a headless run, so choose deliberately."},
	{Name: "sandbox", Kind: "enum", Group: "permissions", Label: "Sandbox", Primary: true,
		Dangerous:   []string{"danger-full-access"},
		Placeholder: "read-only",
		Help:        "What the agent's commands may touch: nothing, the workspace, or the whole machine. Grok takes a profile name its own config may define."},
	{Name: "always_approve", Kind: "bool", Group: "permissions", Label: "Approve every tool call",
		Dangerous: []string{"true"},
		Help:      "Run every tool the agent asks for without confirmation, whatever the permission mode says."},
	{Name: "no_plan", Kind: "bool", Group: "permissions", Label: "Disable plan mode",
		Help: "Stop the agent from planning before it acts, which is usually not what you want unattended."},
	{Name: "no_subagents", Kind: "bool", Group: "permissions", Label: "No subagents",
		Help: "Stop the agent from delegating work to subagents it spawns itself."},
	{Name: "disable_web_search", Kind: "bool", Group: "permissions", Label: "No web access",
		Help: "Remove the web search and fetch tools, so the run cannot reach the internet."},
	{Name: "restricted", Kind: "bool", Group: "permissions", Label: "Restricted mode",
		Help: "Remove every tool that runs commands or code, and confine file access to the allowed directories. The safest way to expose an agent."},
	{Name: "allowed_tools", Kind: "list", Group: "permissions", Label: "Allowed tools",
		Placeholder: "Bash(git *), Edit",
		Help:        "Only these tools may be used, with optional argument patterns."},
	{Name: "disallowed_tools", Kind: "list", Group: "permissions", Label: "Disallowed tools",
		Placeholder: "WebFetch",
		Help:        "Tools the agent may never use, whatever else it is allowed."},
	{Name: "tools", Kind: "list", Group: "permissions", Label: "Tool set",
		Placeholder: "Bash, Edit, Read",
		Help:        "Replace the built-in tool set with exactly these. An empty list leaves the agent with no tools at all."},
	{Name: "approve_for_me", Kind: "bool", Group: "permissions", Label: "Auto-approve",
		Help: "Answer the agent's approval requests automatically, reviewed inside the workspace sandbox."},
	{Name: "safe_mode", Kind: "bool", Group: "permissions", Label: "Safe mode",
		Help: "Disable every customization — memory files, skills, plugins, hooks — for a clean, predictable run."},
	{Name: "disable_slash_commands", Kind: "bool", Group: "permissions", Label: "No slash commands",
		Help: "Turn off the skills a prompt can invoke by name, leaving only the plain assistant."},
	{Name: "dangerously_skip_permissions", Kind: "bool", Group: "permissions", Label: "Skip all permission checks",
		Dangerous: []string{"true"},
		Help:      "The agent acts without asking, at all. Only for an environment you are willing to lose."},
	{Name: "allow_dangerously_skip_permissions", Kind: "bool", Group: "permissions", Label: "Offer to skip checks",
		Dangerous: []string{"true"},
		Help:      "Make skipping permission checks available to the session without turning it on by default."},
	{Name: "dangerously_bypass_approvals_and_sandbox", Kind: "bool", Group: "permissions", Label: "Bypass sandbox and approvals",
		Dangerous: []string{"true"},
		Help:      "Run commands with no sandbox and no confirmation. Only inside something else that is already sandboxed."},
	{Name: "dangerously_bypass_hook_trust", Kind: "bool", Group: "permissions", Label: "Bypass hook trust",
		Dangerous: []string{"true"},
		Help:      "Run configured hooks without the usual check that their source is trusted."},

	// -------------------------------------------------------------- stream
	{Name: "include_partial_messages", Kind: "bool", Group: "stream", Label: "Token-by-token deltas",
		Help: "Emit each fragment as the model writes it, rather than whole messages. Streaming only."},
	{Name: "include_hook_events", Kind: "bool", Group: "stream", Label: "Include hook events",
		Help: "Also emit the lifecycle events of any hooks configured on this machine."},
	{Name: "forward_subagent_text", Kind: "bool", Group: "stream", Label: "Include subagent text",
		Help: "Emit what delegated subagents say and think, not only the main thread's own output."},
	{Name: "prompt_suggestions", Kind: "bool", Group: "stream", Label: "Suggest a next prompt",
		Help: "After the answer, emit a predicted follow-up prompt you might want to send next."},
	{Name: "verbose", Kind: "bool", Group: "stream", Label: "Verbose",
		Help: "Ask the CLI for its chatty output; streaming turns this on anyway."},
	{Name: "debug", Kind: "string", Group: "stream", Label: "Debug",
		Placeholder: `"true", or api,hooks`,
		Help:        "Turn on the CLI's debug logging, optionally filtered by category. It lands in the reply's stderr."},
	{Name: "args", Kind: "list", Group: "stream", Label: "Extra CLI flags",
		Placeholder: "--some-flag, value",
		Help:        "Passed to the vendor CLI verbatim, for anything rota does not model. Flags rota sets itself are refused."},
}

// Fields describes what a Spec may carry for one provider ("" for every
// field of every provider).
//
// Everything a provider decides — which models, which efforts, which
// permission modes, whether it sandboxes at all — is asked of lib rather
// than written down here, so a form always offers exactly what will be
// accepted. With no provider named the enums are dropped instead of merged:
// a union of values from four CLIs would offer combinations none of them
// takes.
func Fields(provider string) []Field {
	flavor := rota.Flavor(provider)
	out := make([]Field, 0, len(specFields))
	for _, f := range specFields {
		f.Flavors = rota.FlavorsOf(f.Name)
		if provider != "" && len(f.Flavors) > 0 && !slices.Contains(f.Flavors, flavor) {
			continue
		}
		switch f.Name {
		case "model":
			for _, m := range rota.Models(provider) {
				f.Enum = append(f.Enum, m.ID)
			}
			f.Default, _ = rota.Defaults(provider)
		case "effort":
			f.Enum = rota.Efforts(provider)
			_, f.Default = rota.Defaults(provider)
			if provider != "" && len(f.Enum) == 0 {
				continue // this provider has no effort setting: do not offer one
			}
		case "permission_mode":
			f.Enum = rota.PermissionModes(provider)
		case "sandbox":
			if provider != "" && !rota.TakesSandbox(provider) {
				continue
			}
			f.Enum = rota.Sandboxes(provider)
		}
		if provider == "" {
			f.Enum, f.Default = nil, ""
			if f.Kind == "enum" {
				f.Kind = "string"
			}
		} else if f.Kind == "enum" && len(f.Enum) == 0 {
			// A provider that accepts a name rota cannot enumerate — grok's
			// sandbox profiles come from its own config — takes free text.
			f.Kind = "string"
		}
		f.Enum = slices.Clone(f.Enum)
		out = append(out, f)
	}
	return out
}
