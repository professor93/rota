package rota

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Spec is one headless request to a vendor CLI, in rota's own vocabulary.
//
// One thing a caller can ask for is one field, whatever the CLIs call it:
// where two of them mean the same thing under different flags, the argv
// builders do the translating, as they already do for effort. A field
// genuinely peculiar to one CLI is named after that CLI's flag, and Argv
// refuses it elsewhere rather than dropping it silently.
type Spec struct {
	// Shared
	Prompt string `json:"prompt"`
	// Send each event as it happens instead of one answer at the end.
	Stream bool `json:"stream,omitzero"`
	// Where the agent runs and which files it sees.
	Cwd string `json:"cwd,omitempty"`
	// Further directories the agent may read and write, beyond the working directory.
	AddDirs []string `json:"add_dirs,omitempty"`
	// ScratchDir is where Argv-created temp files land (a grok prompt, a
	// codex schema). Empty means the system default; a confined server sets
	// it inside a root, and it is checked like every other path.
	ScratchDir string `json:"scratch_dir,omitempty"`
	// Give up after this long and kill the CLI.
	TimeoutSeconds int `json:"timeout_seconds,omitzero"`
	// Include the CLI's whole event stream in the reply, not just the outcome.
	IncludeEvents bool `json:"include_events,omitzero"`
	// Passed to the vendor CLI verbatim, for anything rota does not model.
	Extra []string `json:"args,omitempty"`

	// Claude Code
	Model string `json:"model,omitempty"`
	// How hard the model thinks before answering.
	Effort string `json:"effort,omitempty"`
	// Used when the chosen model is overloaded or unavailable, so a long job does not simply fail.
	FallbackModel string `json:"fallback_model,omitempty"`
	// Force the final answer to match this JSON Schema, so it can be parsed rather than read.
	JSONSchema json.RawMessage `json:"json_schema,omitzero"`
	// Stop the run once it has spent this much.
	MaxBudgetUSD float64 `json:"max_budget_usd,omitzero"`
	// Name a brand-new conversation, so you can resume it later by this id.
	SessionID string `json:"session_id,omitempty"`
	// Continue an earlier conversation, with everything it remembers.
	Resume string `json:"resume,omitempty"`
	// Resume the most recent conversation in the working directory, without having to know its id.
	Continue bool `json:"continue,omitzero"`
	// Branch off the resumed conversation into a new one, leaving the original untouched.
	ForkSession bool `json:"fork_session,omitzero"`
	// Write no session files at all, so the run leaves nothing behind and cannot be resumed afterwards — right for a stateless request.
	Ephemeral bool `json:"ephemeral,omitzero"`
	// Run claude in a throwaway config directory: nothing from the shared
	// home — identity, memory, skills, settings files — reaches the run,
	// and the directory is removed when it ends. Billing always follows
	// the token; this makes the model's context follow it too.
	Hermetic bool `json:"hermetic,omitzero"`
	// A label for this session, shown in the CLI's own session picker.
	Name string `json:"name,omitempty"`
	// Replace the CLI's own system prompt entirely.
	SystemPrompt string `json:"system_prompt,omitempty"`
	// Add instructions after the CLI's own system prompt, keeping its behaviour and adding yours.
	AppendSystemPrompt string `json:"append_system_prompt,omitempty"`
	// A settings object applied to this run only, or a path to a settings file.
	Settings json.RawMessage `json:"settings,omitzero"`
	// Which settings files on the machine to honour.
	SettingSources []string `json:"setting_sources"`
	// Define subagents this run may delegate work to, each with its own instructions.
	Agents json.RawMessage `json:"agents,omitzero"`
	// Run as one of the named agents above, rather than as the default assistant.
	Agent string `json:"agent,omitempty"`
	// Model Context Protocol servers to connect: a path to a config file, or the config itself as JSON.
	MCPConfig []json.RawMessage `json:"mcp_config,omitzero"`
	// Ignore every MCP server configured elsewhere on this machine, using only the ones named above.
	StrictMCPConfig bool `json:"strict_mcp_config,omitzero"`
	// Load plugins from these directories for this run only, without installing them.
	PluginDirs []string `json:"plugin_dirs,omitempty"`
	// Fetch plugin archives from these URLs and load them for this run only.
	PluginURLs []string `json:"plugin_urls,omitempty"`
	// When to summarise the conversation to free context: "auto", or a token count.
	Autocompact string `json:"autocompact,omitempty"`
	// Move machine-specific details out of the system prompt, so repeated calls share a prompt cache.
	ExcludeDynamicSections bool `json:"exclude_dynamic_system_prompt_sections,omitzero"`
	// Run inside a fresh git worktree, so the agent's edits stay off your branch.
	Worktree string `json:"worktree,omitempty"`
	// How the agent asks before acting: plan only, accept edits, or ask for nothing.
	PermissionMode string `json:"permission_mode,omitempty"`
	// Only these tools may be used, with optional argument patterns.
	AllowedTools []string `json:"allowed_tools,omitempty"`
	// Tools the agent may never use, whatever else it is allowed.
	DisallowedTools []string `json:"disallowed_tools,omitempty"`
	// Replace the built-in tool set with exactly these.
	Tools []string `json:"tools"`
	// DangerouslySkipPermissions and AllowDangerouslySkipPermissions need
	// Limits.AllowDangerous, as does PermissionMode "bypassPermissions".
	DangerouslySkipPermissions bool `json:"dangerously_skip_permissions,omitzero"`
	// Make skipping permission checks available to the session without turning it on by default.
	AllowDangerouslySkipPermissions bool `json:"allow_dangerously_skip_permissions,omitzero"`
	// Remove every tool that runs commands or code, and confine file access to the allowed directories.
	Restricted bool `json:"restricted,omitzero"`
	// Disable every customization — memory files, skills, plugins, hooks — for a clean, predictable run.
	SafeMode bool `json:"safe_mode,omitzero"`
	// Turn off the skills a prompt can invoke by name, leaving only the plain assistant.
	DisableSlashCommands bool `json:"disable_slash_commands,omitzero"`
	// Emit each fragment as the model writes it, rather than whole messages.
	IncludePartialMessages bool `json:"include_partial_messages,omitzero"`
	// Also emit the lifecycle events of any hooks configured on this machine.
	IncludeHookEvents bool `json:"include_hook_events,omitzero"`
	// Emit what delegated subagents say and think, not only the main thread's own output.
	ForwardSubagentText bool `json:"forward_subagent_text,omitzero"`
	// After the answer, emit a predicted follow-up prompt you might want to send next.
	PromptSuggestions bool `json:"prompt_suggestions,omitzero"`
	// Ask the CLI for its chatty output; streaming turns this on anyway.
	Verbose bool `json:"verbose,omitzero"`
	// Turn on the CLI's debug logging, optionally filtered by category.
	Debug string `json:"debug,omitempty"`

	// Grok
	Rules string `json:"rules,omitempty"`
	// Stop after this many agent turns, whether or not the job is finished.
	MaxTurns int `json:"max_turns,omitzero"`
	// Remove the web search and fetch tools, so the run cannot reach the internet.
	DisableWebSearch bool `json:"disable_web_search,omitzero"`
	// Stop the agent from planning before it acts, which is usually not what you want unattended.
	NoPlan bool `json:"no_plan,omitzero"`
	// Stop the agent from delegating work to subagents it spawns itself.
	NoSubagents bool `json:"no_subagents,omitzero"`
	// Pass the prompt through exactly as written, with none of the CLI's own preprocessing.
	Verbatim bool `json:"verbatim,omitzero"`
	// Run every tool the agent asks for without confirmation, whatever the permission mode says.
	AlwaysApprove bool `json:"always_approve,omitzero"`

	// Codex
	Sandbox string `json:"sandbox,omitempty"`
	// Answer the agent's approval requests automatically, reviewed inside the workspace sandbox.
	ApproveForMe bool `json:"approve_for_me,omitzero"`
	// Run commands with no sandbox and no confirmation.
	BypassApprovalsAndSandbox bool `json:"dangerously_bypass_approvals_and_sandbox,omitzero"`
	// Run configured hooks without the usual check that their source is trusted.
	BypassHookTrust bool `json:"dangerously_bypass_hook_trust,omitzero"`
	// Names of files uploaded above, to attach to the prompt as images the model can see.
	Images []string `json:"images,omitempty"`
	// Layer a named profile from the account's own config directory over its base configuration.
	Profile string `json:"profile,omitempty"`
	// Override individual config values for this run, as key/value pairs.
	Config map[string]string `json:"config,omitempty"`
	// Turn named experimental features on for the duration of this run.
	Enable []string `json:"enable,omitempty"`
	// Turn named features off for the duration of this run.
	Disable []string `json:"disable,omitempty"`
	// Fail if the config file contains anything this version does not recognise, instead of ignoring it.
	StrictConfig bool `json:"strict_config,omitzero"`
	// Run even when the working directory is not a git repository.
	SkipGitRepoCheck bool `json:"skip_git_repo_check,omitzero"`
	// Do not read the account's own config file; credentials are still used.
	IgnoreUserConfig bool `json:"ignore_user_config,omitzero"`
	// Do not load the user or project execution-policy rules that would otherwise apply.
	IgnoreRules bool `json:"ignore_rules,omitzero"`

	// OneShot marks a question asked of an account rather than a session
	// worked in: rota then chooses defaults that make a CLI answer instead
	// of refusing. An ask-style caller sets it, and it changes nothing a
	// caller has chosen explicitly.
	OneShot bool `json:"one_shot,omitzero"`

	// flavorOverride lets a test drive a fake CLI through the claude or
	// codex vocabulary.
	flavorOverride string
	// scratch holds temp files Argv created (a schema, say), removed by Run.
	scratch []string
}

// Limits is what a caller allows a Spec to ask for. A nil Limits allows
// everything except what the CLI itself refuses — right for a local command,
// wrong for a server.
type Limits struct {
	// Roots confines every path the request names. Empty means unconfined.
	Roots []string
	// AllowDangerous permits permission-bypass and full-access options.
	AllowDangerous bool
	// AllowRawFlags re-opens Extra for a caller that is trusted with the
	// machine anyway. It is off by default because a vendor's flags are not
	// rota's to keep track of: any gate rota adds has a flag that undoes it.
	AllowRawFlags bool
	// SettingsDenyKeys are the settings keys a mediated caller may not send
	// inline. nil means the built-in default (env, apiKeyHelper,
	// awsAuthRefresh, awsCredentialExport, hooks); an operator that sets the
	// field owns the whole policy, replacement rather than extension.
	SettingsDenyKeys []string
	// MaxBufferedOutput, MaxEventLine, MaxStderr (bytes) and MaxEvents
	// (count) override the output bounds; zero keeps the defaults (64MB,
	// 8MB, 64KB, 20000). A run that hits any bound reports Truncated.
	MaxBufferedOutput int
	MaxEventLine      int
	MaxStderr         int
	MaxEvents         int
}

// Result is what one finished run produced.
type Result struct {
	Account  int    `json:"account"`
	Provider string `json:"provider"`
	// Model and Effort are what actually ran, not what was asked for: an
	// empty request field means the provider's default, and a caller cannot
	// see a default from the outside. Both are always present, empty
	// included: an empty effort is the answer for a provider that has no
	// such setting, and a missing field would make a client guess.
	Model      string          `json:"model"`
	Effort     string          `json:"effort"`
	SessionID  string          `json:"session_id,omitempty"`
	Result     string          `json:"result"`
	Structured json.RawMessage `json:"structured_output,omitzero"`
	IsError    bool            `json:"is_error"`
	// Truncated reports that an output bound was hit: the reply is real but
	// shortened, and a caller that needs everything raises the bound.
	Truncated  bool              `json:"truncated,omitzero"`
	Subtype    string            `json:"subtype,omitempty"`
	NumTurns   int               `json:"num_turns,omitzero"`
	CostUSD    float64           `json:"cost_usd,omitzero"`
	Usage      json.RawMessage   `json:"usage,omitzero"`
	DurationMS int64             `json:"duration_ms,omitzero"`
	ExitCode   int               `json:"exit_code"`
	Stderr     string            `json:"stderr,omitempty"`
	Events     []json.RawMessage `json:"events,omitzero"`
}

// Flavored is implemented by a provider that names the CLI vocabulary it
// speaks, so a third-party provider driving Claude Code gets claude's argv
// builder rather than the builtin name table answering "unknown".
type Flavored interface {
	Flavor() string
}

// Flavor is the CLI vocabulary a provider speaks. Several providers can
// share one: a provider that drives Claude Code takes Claude Code's flags,
// whatever its own models are. A registered provider may say so itself, via
// Flavored; the builtin names answer without being registered at all.
func Flavor(provider string) string {
	if p, err := Lookup(provider); err == nil {
		if f, ok := p.(Flavored); ok {
			return f.Flavor()
		}
	}
	switch provider {
	case "claude":
		return "claude"
	case "codex":
		return "codex"
	case "grok":
		return "grok"
	case "kimi":
		return "kimi"
	}
	return ""
}

var (
	permModes = []string{"acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan"}
	// grokPermModes is Grok Build's own list: "default" where Claude Code
	// says "manual", and no "manual" at all.
	grokPermModes = []string{"default", "acceptEdits", "auto", "dontAsk", "bypassPermissions", "plan"}
	// kimiPermModes is what kimi's own switch below accepts. It is named
	// rather than written out twice, because a list that disagrees with the
	// code enforcing it is worse than no list.
	kimiPermModes = []string{"plan", "acceptEdits", "dontAsk", "auto", "bypassPermissions"}
	sandboxes     = []string{"read-only", "workspace-write", "danger-full-access"}
	// reservedFlags are the flags rota sets itself: letting a caller pass
	// them through Extra would break output parsing, streaming or auth.
	reservedFlags = []string{"-p", "--print", "--single", "--prompt-file", "--prompt-json",
		"--output-format", "--input-format", "--json", "--color", "-o", "--output-last-message",
		"--bare", "--betas", "-C", "--cd", "--cloud", "--bg", "--background"}
)

func enumErr(field, got string, allowed []string) error {
	return failf(ErrInvalidRequest, "%s %q is not one of: %s", field, got, strings.Join(allowed, ", "))
}

// argv builds the command line for one provider. The provider, not just its
// CLI flavor, decides which models and efforts are valid: two providers can
// drive the same binary and still accept entirely different models.
func (s *Spec) argv(provider string, lim *Limits) ([]string, error) {
	p, err := s.planFor(provider, Models(provider), lim)
	if err != nil {
		return nil, err
	}
	return p.argv, nil
}

// plan is a resolved command line together with the choices rota made to
// build it. Resolving the model and then discarding it is how a result ends
// up unable to say what answered it.
type plan struct {
	argv   []string
	model  string
	effort string
}

// planFor is argv against an explicit model list, which is how an
// account-specific catalog reaches the check.
func (s *Spec) planFor(provider string, models []Model, lim *Limits) (*plan, error) {
	// Supplying limits at all is the signal that this request is being
	// mediated for someone else. A nil Limits is a person at their own
	// terminal, who may pass the CLI whatever they like — they could run it
	// directly anyway.
	// "last" is codex's word for the most recent session. Every other CLI
	// has a flag meaning the same thing and would take "last" for a session
	// id — which is what they did. It becomes that flag here, once, so
	// nothing downstream has to know the word.
	if s.Resume == "last" {
		s.Continue, s.Resume = true, ""
	}
	mediated := lim != nil
	if lim == nil {
		lim = &Limits{}
	}
	if strings.TrimSpace(s.Prompt) == "" {
		return nil, failf(ErrInvalidRequest, "prompt is required")
	}
	if err := s.checkExtra(mediated, lim.AllowRawFlags); err != nil {
		return nil, err
	}
	if err := s.checkSuppliedConfig(mediated, lim, lim.AllowRawFlags); err != nil {
		return nil, err
	}
	set, err := s.fieldsSet()
	if err != nil {
		return nil, err
	}
	flavor := Flavor(provider)
	if err := checkFlavor(provider, flavor, set); err != nil {
		return nil, err
	}
	if err := s.checkPaths(flavor, lim); err != nil {
		return nil, err
	}
	if s.ScratchDir != "" {
		// Resolved at check time and used resolved, so a symlink repointed
		// between the two cannot move the scratch file outside the roots.
		if resolved, err := realPath(s.ScratchDir); err == nil {
			s.ScratchDir = resolved
		}
	}
	model, err := resolveModel(provider, s.Model, models)
	if err != nil {
		return nil, err
	}
	effort, err := ResolveEffort(provider, s.Effort)
	if err != nil {
		return nil, err
	}
	var argv []string
	switch flavor {
	case "claude":
		argv, err = s.claudeArgv(model, effort, lim)
	case "codex":
		argv, err = s.codexArgv(model, effort, lim)
	case "grok":
		argv, err = s.grokArgv(model, effort, lim)
	case "kimi":
		argv, err = s.kimiArgv(model, lim)
		effort = "" // kimi has no effort setting, so it reports none
	default:
		return nil, unsupported(provider)
	}
	if err != nil {
		return nil, err
	}
	return &plan{argv: argv, model: model, effort: effort}, nil
}

func unsupported(provider string) error {
	return failf(ErrUnsupported,
		"no headless interface is modelled for %q, so a command line cannot be built for it; "+
			"launch its CLI directly, handing it the account's own flags", provider)
}

// fieldsSet lists the request fields this spec actually carries, by their
// wire names. It is read back out of the encoded form so the list can never
// drift from the struct: a new field is covered the moment it exists.
func (s *Spec) fieldsSet() ([]string, error) {
	raw, err := Encode(s)
	if err != nil {
		return nil, failf(ErrInvalidRequest, "cannot read the request: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := jsonv2.Unmarshal(raw, &doc); err != nil {
		return nil, failf(ErrInvalidRequest, "cannot read the request: %v", err)
	}
	set := make([]string, 0, len(doc))
	for name, v := range doc {
		// A field without omitempty is present even when it holds nothing;
		// "null" and "false" mean the caller did not ask for it.
		if string(v) == "null" || string(v) == "false" {
			continue
		}
		set = append(set, name)
	}
	return set, nil
}

func (s *Spec) checkExtra(mediated, allowRaw bool) error {
	if len(s.Extra) > 0 && mediated && !allowRaw {
		// Every option rota gates — skipping permissions, a full-access
		// sandbox, another directory — is also a flag the vendor CLI takes.
		// Filtering a list of them would be a game rota loses on the next
		// CLI release, so a confined caller does not get raw flags at all.
		return failf(ErrInvalidRequest,
			"args is not available here: a raw CLI flag would bypass the checks this caller is subject to")
	}
	for _, a := range s.Extra {
		name, _, _ := strings.Cut(a, "=")
		if slices.Contains(reservedFlags, name) {
			return failf(ErrInvalidRequest, "args may not contain %s: rota sets it", name)
		}
	}
	return nil
}

// checkPaths confines every field that names something on disk to the allowed
// roots, and insists a directory exists, so a caller cannot reach outside them
// or fail obscurely later.
//
// The list must stay exhaustive, and TestEveryPathFieldIsConfined is what
// keeps it honest: a field rota forgets here is one a caller can point at
// the token store, because a coding agent reading a file and describing it
// is an exfiltration primitive.
//
// It takes the flavor because the same field is not a path everywhere: what
// grok writes a debug log to, Claude Code reads as a category filter.
// nonEmpty is a one-or-none list, for optional single paths.
func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func (s *Spec) checkPaths(flavor string, lim *Limits) error {
	dirs := []struct {
		what string
		list []string
	}{
		{"cwd", nil}, // handled below, it may be empty
		{"add_dirs", s.AddDirs},
		{"plugin_dirs", s.PluginDirs},
		{"scratch_dir", nonEmpty(s.ScratchDir)},
	}
	if s.Cwd != "" {
		dirs[0].list = []string{s.Cwd}
	}
	for _, group := range dirs {
		for _, d := range group.list {
			if err := checkDir(group.what, d, lim.Roots); err != nil {
				return err
			}
		}
	}
	files := []struct {
		what string
		list []string
	}{
		{"images", s.Images},
		{"mcp_config", pathsOf(s.MCPConfig)},
		{"settings", pathsOf([]json.RawMessage{s.Settings})},
	}
	// grok writes its debug log wherever it is told, which is a write
	// primitive rather than a read one. Claude Code's --debug is a category
	// filter and writes to a file only through a flag rota does not model, so
	// confining it there would refuse a value that names nothing on disk.
	if flavor == "grok" && s.Debug != "" && s.Debug != "true" {
		files = append(files, struct {
			what string
			list []string
		}{"debug", []string{s.Debug}})
	}
	for _, group := range files {
		for _, f := range group.list {
			if err := checkPath(group.what, f, lim.Roots); err != nil {
				return err
			}
		}
	}
	return nil
}

// pathsOf picks the JSON values that are plain strings, which is how a
// caller names a file rather than supplying the document inline.
func pathsOf(docs []json.RawMessage) []string {
	var out []string
	for _, d := range docs {
		var str string
		if len(d) > 0 && decodeLenient(d, &str) == nil && str != "" {
			out = append(out, str)
		}
	}
	return out
}

// checkPath confines a file the same way checkDir confines a directory.
func checkPath(what, path string, roots []string) error {
	if path == "" {
		return nil
	}
	abs, err := realPath(path)
	if err != nil {
		return failf(ErrInvalidRequest, "%s %q: %v", what, path, err)
	}
	if len(roots) == 0 {
		return nil
	}
	for _, root := range roots {
		if r, err := realPath(root); err == nil && within(r, abs) {
			return nil
		}
	}
	return failf(ErrOutsideRoots, "%s %q is outside the allowed directories", what, path)
}

// checkSuppliedConfig refuses the configuration and code a confined caller
// may not hand the child.
//
// They are one check because they are one act. Claude Code's settings file may
// carry an `env` block, an `apiKeyHelper` command and hooks; an inline MCP
// server is a command line with its own environment; a plugin fetched from a
// URL carries hooks of its own; and a codex config override names the endpoint
// the run is sent to. Each of them undoes the careful work of deciding what
// the child may see and where its credential goes — an `ANTHROPIC_BASE_URL`
// set in settings, or a `model_providers.x.base_url` set in config, sends the
// token and the whole conversation wherever the caller likes.
//
// What is left is what the operator chose: a path to a settings file, a plugin
// directory inside the roots. A caller trusted with the machine keeps the rest
// through AllowRawFlags, which is the same trust by a different name.
func (s *Spec) checkSuppliedConfig(mediated bool, lim *Limits, allowRaw bool) error {
	if !mediated {
		return nil
	}
	deny := lim.SettingsDenyKeys
	if deny == nil {
		deny = []string{"env", "apiKeyHelper", "awsAuthRefresh", "awsCredentialExport", "hooks",
			"permissions", "otelHeadersHelper", "statusLine", "forceLoginMethod"}
	}
	vet := func(doc map[string]json.RawMessage) error {
		for _, key := range deny {
			if _, found := doc[key]; found {
				return failf(ErrInvalidRequest,
					"settings may not carry %q here: it would give the agent an environment rota did not choose", key)
			}
		}
		return nil
	}
	if doc := inlineObject(s.Settings); doc != nil {
		if err := vet(doc); err != nil {
			return err
		}
	} else if paths := pathsOf([]json.RawMessage{s.Settings}); len(paths) == 1 {
		// A path is the same document one indirection later — and the roots
		// it must live under include the caller's own uploads, so the file is
		// as caller-supplied as an inline object. Read it and hold it to the
		// same rule.
		doc, err := readConfigFile(paths[0])
		if err != nil {
			return err
		}
		if err := vet(doc); err != nil {
			return err
		}
	}
	for _, m := range s.MCPConfig {
		doc := inlineObject(m)
		if doc == nil {
			if paths := pathsOf([]json.RawMessage{m}); len(paths) == 1 {
				var err error
				if doc, err = readConfigFile(paths[0]); err != nil {
					return err
				}
				// A file of url-only servers is configuration; one that names
				// a command or an environment is a program launch, which is
				// what this gate exists to keep out of a caller's hands.
				if err := vetMCPServers(doc); err != nil {
					return err
				}
				continue
			}
		}
		if doc != nil {
			return failf(ErrInvalidRequest,
				"mcp_config must be a path here: an inline server is a command line with an environment of its own")
		}
	}
	// Which settings files on the machine a run may read is the operator's
	// call, not the caller's: nil and the explicit empty list pass, anything
	// named does not.
	if !allowRaw && len(s.SettingSources) > 0 {
		return failf(ErrInvalidRequest,
			"setting_sources is not available here: it points the run at settings files in the workspace, which carry the keys refused above")
	}
	if allowRaw {
		// The two below are the same trust as a raw flag, and a caller holding
		// that could pass --plugin-url or -c itself. Refusing them here would
		// be a gate with a hole beside it.
		return nil
	}
	// A plugin is code, and a plugin's hooks are commands run with the agent's
	// own environment — the thing refused just above. A directory is confined
	// to the roots the operator chose; a URL is not confined at all.
	if len(s.PluginURLs) > 0 {
		return failf(ErrInvalidRequest,
			"plugin_urls is not available here: a plugin is code fetched from wherever the URL points, "+
				"and its hooks run with the agent's environment; use plugin_dirs inside an allowed directory")
	}
	// config is codex's own configuration, and rota gates nothing inside it:
	// the keys reach the CLI whole. Naming the dangerous ones would be a list
	// the next codex release outgrows — it already has a key for the endpoint
	// to talk to, and a key for a program to run on an event.
	if len(s.Config) > 0 {
		return failf(ErrInvalidRequest,
			"config is not available here: a config override can name the endpoint the run is sent to, "+
				"which would send the prompt and the credential somewhere rota did not choose")
	}
	return nil
}

// readConfigFile loads a caller-named configuration file for vetting, bounded
// so a huge file cannot be used to stall the check.
func readConfigFile(path string) (map[string]json.RawMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, failf(ErrInvalidRequest, "config file %q: %v", path, err)
	}
	if len(raw) > 1<<20 {
		return nil, failf(ErrInvalidRequest, "config file %q is over 1MB, which no settings document is", path)
	}
	var doc map[string]json.RawMessage
	if err := decodeLenient(raw, &doc); err != nil {
		return nil, failf(ErrInvalidRequest, "config file %q: %v", path, err)
	}
	return doc, nil
}

// vetMCPServers refuses any server entry that names a command or an
// environment, wherever the document keeps its servers.
func vetMCPServers(doc map[string]json.RawMessage) error {
	servers := doc
	if raw, ok := doc["mcpServers"]; ok {
		servers = inlineObject(raw)
	}
	for name, raw := range servers {
		entry := inlineObject(raw)
		if entry == nil {
			continue
		}
		for _, banned := range []string{"command", "env", "args"} {
			if _, found := entry[banned]; found {
				return failf(ErrInvalidRequest,
					"mcp_config server %q names a %s: a server that runs a program is a command line with an environment of its own, which is not available here", name, banned)
			}
		}
	}
	return nil
}

// inlineObject decodes a JSON object supplied in the request itself, or nil
// when the value is a string naming a file.
func inlineObject(raw json.RawMessage) map[string]json.RawMessage {
	var doc map[string]json.RawMessage
	if len(raw) == 0 || decodeLenient(raw, &doc) != nil {
		return nil
	}
	return doc
}

func checkDir(what, dir string, roots []string) error {
	abs, err := realPath(dir)
	if err != nil {
		return failf(ErrInvalidRequest, "%s %q: %v", what, dir, err)
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return failf(ErrInvalidRequest, "%s %q is not an existing directory", what, dir)
	}
	if len(roots) == 0 {
		return nil
	}
	for _, root := range roots {
		if r, err := realPath(root); err == nil && within(r, abs) {
			return nil
		}
	}
	return failf(ErrOutsideRoots, "%s %q is outside the allowed directories", what, dir)
}

// realPath resolves a path the way the filesystem will, symlinks included,
// so a confinement check cannot be defeated by a link — and does not fail
// spuriously where a system links its own directories, as macOS links /tmp
// and /var into /private.
func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// within reports whether path is root or inside it, without being fooled by
// a shared name prefix ("/srv/data2" is not inside "/srv/data").
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *Spec) dangerous(lim *Limits, what string) error {
	if lim.AllowDangerous {
		return nil
	}
	return failf(ErrDangerous, "%s is a dangerous option; the caller did not allow it", what)
}

func (s *Spec) claudeArgv(model, effort string, lim *Limits) ([]string, error) {
	a := []string{"-p"}
	if s.Stream {
		a = append(a, "--output-format", "stream-json", "--verbose")
	} else {
		a = append(a, "--output-format", "json")
	}
	// nil leaves the CLI's own settings sources alone; an explicit list —
	// empty included — is passed through. A server that wants a hermetic
	// session sets [] itself; that is its policy, not the SDK's.
	if s.SettingSources != nil {
		a = append(a, "--setting-sources", strings.Join(s.SettingSources, ","))
	}

	str := func(flag, v string) {
		if v != "" {
			a = append(a, flag, v)
		}
	}
	flag := func(f string, on bool) {
		if on {
			a = append(a, f)
		}
	}
	list := func(f string, v []string, sep bool) {
		if v == nil {
			return
		}
		if sep {
			a = append(a, f, strings.Join(v, ","))
			return
		}
		for _, x := range v {
			a = append(a, f, x)
		}
	}
	raw := func(f string, v json.RawMessage) {
		if len(v) > 0 {
			a = append(a, f, string(v))
		}
	}

	str("--model", model)
	str("--effort", effort)
	str("--fallback-model", s.FallbackModel)
	raw("--json-schema", s.JSONSchema)
	if s.MaxBudgetUSD > 0 {
		a = append(a, "--max-budget-usd", strconv.FormatFloat(s.MaxBudgetUSD, 'f', -1, 64))
	}
	str("--session-id", s.SessionID)
	str("--resume", s.Resume)
	flag("-c", s.Continue)
	flag("--fork-session", s.ForkSession)
	flag("--no-session-persistence", s.Ephemeral)
	str("--name", s.Name)
	str("--system-prompt", s.SystemPrompt)
	str("--append-system-prompt", s.AppendSystemPrompt)
	raw("--settings", s.Settings)
	raw("--agents", s.Agents)
	str("--agent", s.Agent)
	if len(s.MCPConfig) > 0 {
		// Claude Code takes either a path or an inline JSON document here,
		// so both a JSON string and a JSON object are accepted and reach it
		// in the form it expects.
		a = append(a, "--mcp-config")
		for _, m := range s.MCPConfig {
			var path string
			if decodeLenient(m, &path) == nil {
				a = append(a, path)
			} else {
				a = append(a, string(m))
			}
		}
	}
	flag("--strict-mcp-config", s.StrictMCPConfig)
	list("--plugin-dir", s.PluginDirs, false)
	list("--plugin-url", s.PluginURLs, false)
	str("--autocompact", s.Autocompact)
	flag("--exclude-dynamic-system-prompt-sections", s.ExcludeDynamicSections)
	if s.Worktree != "" {
		if s.Worktree == "true" {
			a = append(a, "--worktree")
		} else {
			a = append(a, "--worktree", s.Worktree)
		}
	}
	if s.PermissionMode != "" {
		if !slices.Contains(permModes, s.PermissionMode) {
			return nil, enumErr("permission_mode", s.PermissionMode, permModes)
		}
		if s.PermissionMode == "bypassPermissions" {
			if err := s.dangerous(lim, "permission_mode bypassPermissions"); err != nil {
				return nil, err
			}
		}
		a = append(a, "--permission-mode", s.PermissionMode)
	}
	if s.DangerouslySkipPermissions {
		if err := s.dangerous(lim, "dangerously_skip_permissions"); err != nil {
			return nil, err
		}
		a = append(a, "--dangerously-skip-permissions")
	}
	if s.AllowDangerouslySkipPermissions {
		if err := s.dangerous(lim, "allow_dangerously_skip_permissions"); err != nil {
			return nil, err
		}
		a = append(a, "--allow-dangerously-skip-permissions")
	}
	list("--allowedTools", s.AllowedTools, true)
	list("--disallowedTools", s.DisallowedTools, true)
	list("--tools", s.Tools, true)
	flag("--restricted", s.Restricted)
	flag("--safe-mode", s.SafeMode)
	flag("--disable-slash-commands", s.DisableSlashCommands)
	flag("--include-partial-messages", s.IncludePartialMessages)
	flag("--include-hook-events", s.IncludeHookEvents)
	flag("--forward-subagent-text", s.ForwardSubagentText)
	if s.PromptSuggestions {
		a = append(a, "--prompt-suggestions", "true")
	}
	if s.Verbose && !s.Stream {
		a = append(a, "--verbose")
	}
	if s.Debug != "" {
		if s.Debug == "true" {
			a = append(a, "--debug")
		} else {
			a = append(a, "--debug", s.Debug)
		}
	}
	list("--add-dir", s.AddDirs, false)
	return append(a, s.Extra...), nil
}

func (s *Spec) codexArgv(model, effort string, lim *Limits) ([]string, error) {
	if s.OneShot {
		// codex refuses to run outside a git repository, which protects a
		// person from an agent editing files they cannot roll back. A
		// question asked read-only writes nothing, so there is nothing for
		// that rule to protect — and refusing to answer would be the
		// surprise, not the safety.
		if s.Sandbox == "" {
			s.Sandbox = "read-only"
		}
		s.SkipGitRepoCheck = true
	}
	a := []string{"exec"}
	switch {
	case s.Resume != "" && s.ForkSession:
		a = append(a, "fork", s.Resume)
	case s.Continue:
		// Every other CLI has a flag for carrying on from the last session.
		// codex has a subcommand, which is its business, not the caller's.
		if s.ForkSession {
			a = append(a, "fork", "--last")
		} else {
			a = append(a, "resume", "--last")
		}
	case s.Resume != "":
		a = append(a, "resume", s.Resume)
	}
	// "-" makes codex read the prompt from stdin, so no prompt ever lands
	// in the process table.
	a = append(a, "-", "--json", "--color", "never")

	if model != "" {
		a = append(a, "-m", model)
	}
	if effort != "" {
		a = append(a, "-c", "model_reasoning_effort="+effort)
	}
	if s.Sandbox != "" {
		if !slices.Contains(sandboxes, s.Sandbox) {
			return nil, enumErr("sandbox", s.Sandbox, sandboxes)
		}
		if s.Sandbox == "danger-full-access" {
			if err := s.dangerous(lim, "sandbox danger-full-access"); err != nil {
				return nil, err
			}
		}
		a = append(a, "-s", s.Sandbox)
	}
	if s.ApproveForMe {
		a = append(a, "--approve-for-me")
	}
	for _, d := range []struct {
		on   bool
		what string
		flag string
	}{
		{s.BypassApprovalsAndSandbox, "dangerously_bypass_approvals_and_sandbox", "--dangerously-bypass-approvals-and-sandbox"},
		{s.BypassHookTrust, "dangerously_bypass_hook_trust", "--dangerously-bypass-hook-trust"},
	} {
		if d.on {
			if err := s.dangerous(lim, d.what); err != nil {
				return nil, err
			}
			a = append(a, d.flag)
		}
	}
	if s.Profile != "" {
		a = append(a, "-p", s.Profile)
	}
	for _, k := range sortedKeys(s.Config) {
		a = append(a, "-c", k+"="+s.Config[k])
	}
	for _, f := range s.Enable {
		a = append(a, "--enable", f)
	}
	for _, f := range s.Disable {
		a = append(a, "--disable", f)
	}
	if s.StrictConfig {
		a = append(a, "--strict-config")
	}
	if len(s.JSONSchema) > 0 {
		// codex reads the schema from a file rather than the command line.
		path, err := s.temp("schema-*.json", s.JSONSchema)
		if err != nil {
			return nil, err
		}
		a = append(a, "--output-schema", path)
	}
	for _, img := range s.Images {
		a = append(a, "-i", img)
	}
	for _, d := range s.AddDirs {
		a = append(a, "--add-dir", d)
	}
	if s.Ephemeral {
		a = append(a, "--ephemeral")
	}
	if s.SkipGitRepoCheck {
		a = append(a, "--skip-git-repo-check")
	}
	if s.IgnoreUserConfig {
		a = append(a, "--ignore-user-config")
	}
	if s.IgnoreRules {
		a = append(a, "--ignore-rules")
	}
	return append(a, s.Extra...), nil
}

// grokArgv builds a headless Grok Build command line. Its flags are close
// to Claude Code's but not the same: the prompt is a value rather than a
// switch, the system prompt is an "override", allow and deny replace the
// tool lists, and the streaming format has its own name.
func (s *Spec) grokArgv(model, effort string, lim *Limits) ([]string, error) {
	// --prompt-file rather than -p: the prompt is a whole request and must
	// not sit in the process table where any other user could read it.
	path, err := s.temp("prompt-*.txt", []byte(s.Prompt))
	if err != nil {
		return nil, err
	}
	a := []string{"--prompt-file", path}
	if s.Stream {
		// The Anthropic wire format, which rota already parses.
		a = append(a, "--output-format", "streaming-messages-json")
	} else {
		a = append(a, "--output-format", "json")
	}

	str := func(flag, v string) {
		if v != "" {
			a = append(a, flag, v)
		}
	}
	flag := func(f string, on bool) {
		if on {
			a = append(a, f)
		}
	}

	str("--model", model)
	str("--reasoning-effort", effort)
	if len(s.JSONSchema) > 0 {
		a = append(a, "--json-schema", string(s.JSONSchema))
	}
	if s.MaxTurns > 0 {
		a = append(a, "--max-turns", strconv.Itoa(s.MaxTurns))
	}
	str("--session-id", s.SessionID)
	str("--resume", s.Resume)
	flag("--continue", s.Continue)
	flag("--fork-session", s.ForkSession)
	str("--system-prompt-override", s.SystemPrompt)
	str("--rules", s.Rules)
	str("--agent", s.Agent)
	if len(s.Agents) > 0 {
		a = append(a, "--agents", string(s.Agents))
	}
	if s.PermissionMode != "" {
		if !slices.Contains(grokPermModes, s.PermissionMode) {
			return nil, enumErr("permission_mode", s.PermissionMode, grokPermModes)
		}
		if s.PermissionMode == "bypassPermissions" {
			if err := s.dangerous(lim, "permission_mode bypassPermissions"); err != nil {
				return nil, err
			}
		}
		a = append(a, "--permission-mode", s.PermissionMode)
	}
	if s.Sandbox != "" {
		// Grok Build takes a profile *name* here, and a project's own config
		// may define more than the three it ships, so the value is not
		// restricted — only the one that grants everything is gated.
		if s.Sandbox == "danger-full-access" {
			if err := s.dangerous(lim, "sandbox danger-full-access"); err != nil {
				return nil, err
			}
		}
		a = append(a, "--sandbox", s.Sandbox)
	}
	if s.AlwaysApprove {
		if err := s.dangerous(lim, "always_approve"); err != nil {
			return nil, err
		}
		a = append(a, "--always-approve")
	}
	for _, t := range s.AllowedTools {
		a = append(a, "--allow", t)
	}
	for _, t := range s.DisallowedTools {
		a = append(a, "--deny", t)
	}
	if s.Tools != nil {
		a = append(a, "--tools", strings.Join(s.Tools, ","))
	}
	flag("--disable-web-search", s.DisableWebSearch)
	flag("--no-plan", s.NoPlan)
	flag("--no-subagents", s.NoSubagents)
	flag("--verbatim", s.Verbatim)
	flag("--include-partial-messages", s.IncludePartialMessages)
	if s.Worktree != "" {
		if s.Worktree == "true" {
			a = append(a, "--worktree")
		} else {
			a = append(a, "--worktree", s.Worktree)
		}
	}
	if s.Debug != "" {
		if s.Debug == "true" {
			a = append(a, "--debug")
		} else {
			a = append(a, "--debug-file", s.Debug)
		}
	}
	// grok takes its working directory as a flag rather than inheriting it.
	str("--cwd", s.Cwd)
	return append(a, s.Extra...), nil
}

// kimiArgv builds a headless Kimi Code command line. Its vocabulary is the
// smallest of the four: one prompt flag, two output formats, and permission
// settings that are separate switches rather than a mode.
func (s *Spec) kimiArgv(model string, lim *Limits) ([]string, error) {
	// kimi has no stdin path for the prompt: -p takes it as an argument, so
	// this is the one argv a prompt rides on. Run's doc carries the caveat.
	a := []string{"-p", s.Prompt}
	if s.Stream {
		a = append(a, "--output-format", "stream-json")
	} else {
		a = append(a, "--output-format", "text")
	}
	if model != "" {
		a = append(a, "-m", model)
	}
	if s.Resume != "" {
		a = append(a, "-S", s.Resume)
	}
	if s.Continue {
		a = append(a, "-c")
	}
	if s.Agent != "" {
		a = append(a, "--agent", s.Agent)
	}
	switch s.PermissionMode {
	case "":
	case "plan":
		a = append(a, "--plan")
	case "acceptEdits", "dontAsk":
		// -y approves ordinary tool calls but still lets the agent ask.
		a = append(a, "-y")
	case "auto", "bypassPermissions":
		// Both spell --auto, and --auto approves everything: one gate for
		// the one behavior, or the gate on bypassPermissions is decorative.
		if err := s.dangerous(lim, "permission_mode "+s.PermissionMode); err != nil {
			return nil, err
		}
		a = append(a, "--auto")
	default:
		return nil, enumErr("permission_mode", s.PermissionMode, kimiPermModes)
	}
	for _, d := range s.AddDirs {
		a = append(a, "--add-dir", d)
	}
	return append(a, s.Extra...), nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// temp writes scratch data Run will delete once the CLI has exited.
func (s *Spec) temp(pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp(s.ScratchDir, pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Close()); err != nil {
		os.Remove(name)
		return "", err
	}
	s.scratch = append(s.scratch, name)
	return name, nil
}

// cleanup removes whatever Argv staged.
func (s *Spec) cleanup() {
	for _, p := range s.scratch {
		os.Remove(p)
	}
	s.scratch = nil
}

// Run starts an account's CLI with this spec and waits for it to finish.
//
// The prompt is written to the child's stdin, never onto its command line,
// so it cannot leak through the process table — except for kimi, whose CLI
// takes the prompt only as an argument (-p): a kimi prompt is visible in the
// local process table while the run lasts, because the vendor offers no
// other door. Every event line the CLI
// prints is copied to events as it arrives (pass nil to discard, or an
// http.ResponseWriter to stream), and the terminal result event is parsed
// into the returned Result. Cancelling ctx kills the CLI, which is how a
// server stops work when its client goes away.
//
// A nil cmd means rota stages the CLI itself, which is what a caller with
// nothing to coordinate wants. A caller may instead stage first and pass
// what it got: staging is the only part that needs a lock over the caller's
// accounts — it may adopt a token the CLI rotated, and that must reach disk
// before anything runs — while the run itself lasts as long as the agent
// does and needs nothing from a store. Holding a lock across it would
// serialize every other caller behind one agent.
//
// The account may be modified — a rotated token adopted, a refreshed one
// applied — and the caller must persist it. Store.Run does that for you.
func Run(ctx context.Context, a *Account, home string, cmd *Command, spec Spec, lim *Limits, events io.Writer) (*Result, error) {
	defer spec.cleanup()
	if cmd == nil {
		staged, err := Stage(a, home)
		if err != nil {
			return nil, err
		}
		if staged.BaseEnv == nil {
			// Stage cannot invent the child's environment — the SDK never
			// reads its own — and a CLI spawned with only its credential
			// variables has no PATH and no HOME. Refusing loudly beats that.
			return nil, failf(ErrInvalidRequest,
				"Run needs the child's base environment: stage the command yourself and set Command.BaseEnv "+
					"(usually your process environment with your own secrets removed)")
		}
		cmd = staged
	}
	provider := spec.flavorOverride
	if provider == "" {
		provider = a.Provider
	}
	// Building a command line for an account that cannot authenticate only
	// produces a vendor error nobody can act on. Handing the terminal over
	// is a different matter and stays allowed: it is how a delegated
	// account gets signed in.
	if p, err := Lookup(a.Provider); err == nil {
		if c, ok := p.(SignInChecker); ok {
			if err := c.SignedIn(a, home); err != nil {
				return nil, err
			}
		}
	}
	pl, err := spec.planFor(provider, ModelsFor(a, home), lim)
	if err != nil {
		return nil, err
	}
	argv := pl.argv
	path, err := exec.LookPath(cmd.Bin)
	if err != nil {
		return nil, WrapNoBinary(cmd.Bin, err)
	}
	if spec.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(spec.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	child := exec.CommandContext(ctx, path, argv...)
	runCmd := cmd
	if spec.Hermetic {
		// A fresh config dir per run: the shared home's identity and memory
		// cannot reach the context, and nothing of the run outlives it.
		hd, herr := os.MkdirTemp(spec.ScratchDir, "rota-hermetic-")
		if herr != nil {
			return nil, herr
		}
		defer os.RemoveAll(hd)
		hc := *cmd
		hc.Env = append(append([]string(nil), cmd.Env...), "CLAUDE_CONFIG_DIR="+hd)
		runCmd = &hc
	}
	child.Env = Environ(runCmd.BaseEnv, runCmd)
	// The resolved path, not the one the caller wrote: checkDir validated
	// where the symlinks led, and handing the kernel the unresolved string
	// would let one repointed between check and start escape.
	if spec.Cwd != "" {
		if resolved, rerr := realPath(spec.Cwd); rerr == nil {
			child.Dir = resolved
		} else {
			child.Dir = spec.Cwd
		}
	}
	child.Stdin = strings.NewReader(spec.Prompt)
	stdout, err := child.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// A CLI in debug mode can print without limit, and whatever it prints
	// comes back in the reply. Keep the tail, which is where a failure
	// explains itself, and nothing more.
	cp := lim.caps()
	stderr := &tailBuffer{limit: cp.stderr}
	child.Stderr = stderr
	// Kill the whole group: these CLIs spawn helpers that would otherwise
	// outlive a cancelled request.
	setPgid(child)
	if err := child.Start(); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { killGroup(child) })
	defer stop()

	res := &Result{Account: a.ID, Provider: a.Provider, Model: pl.model, Effort: pl.effort}
	start := time.Now()
	scanErr := readOutput(stdout, events, spec.Stream, spec.IncludeEvents, cp, res)
	if errors.Is(scanErr, bufio.ErrTooLong) {
		// One event outgrew the line cap: a bound was hit, not a failure.
		res.Truncated, scanErr = true, nil
		killGroup(child)
		_, _ = io.Copy(io.Discard, stdout)
	} else if scanErr != nil || res.Truncated {
		// Nobody is reading stdout any more, and a child that fills its
		// pipe blocks for ever — Wait would never return. Stop it.
		killGroup(child)
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr := child.Wait()
	res.DurationMS = time.Since(start).Milliseconds()
	res.Stderr = strings.TrimSpace(stderr.String())
	if stderr.dropped > 0 {
		res.Truncated = true // the stderr bound is a bound like the others
	}
	res.ExitCode = child.ProcessState.ExitCode()

	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			res.IsError = true
			if res.Result == "" {
				res.Result = res.Stderr
			}
			return res, nil // the CLI ran and failed: that is a result, not a rota error
		}
		return res, waitErr
	}
	return res, scanErr
}

// readOutput turns whatever the CLI printed into a Result.
//
// A streaming run prints one JSON event per line and must be forwarded as it
// arrives. A buffered one prints a single document — an array of events for
// Claude Code, one object for grok — which may be indented across many
// lines, so reading it a line at a time parses none of it. The two need
// different treatment, and only the caller knows which was asked for.
func readOutput(r io.Reader, out io.Writer, streaming, keep bool, cp caps, res *Result) error {
	if streaming {
		return scanEvents(r, out, keep, cp, res)
	}
	raw, err := io.ReadAll(io.LimitReader(r, cp.buffered+1))
	if int64(len(raw)) > cp.buffered {
		raw, res.Truncated = raw[:cp.buffered], true
	}
	if err != nil {
		return err
	}
	if out != nil {
		if _, err := out.Write(raw); err != nil {
			return err
		}
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && jsontext.Value(trimmed).IsValid() {
		doc := json.RawMessage(trimmed)
		if keep && trimmed[0] == '{' {
			res.Events = append(res.Events, doc)
		}
		absorb(doc, keep, cp, res)
		return nil
	}
	// Not one document: either JSONL, or a CLI printing prose.
	return scanEvents(bytes.NewReader(raw), nil, keep, cp, res)
}

// maxBufferedOutput bounds a non-streaming reply. It is generous because the
// whole conversation can come back in one document.
const maxBufferedOutput = 64 << 20

// scanEvents copies each JSONL event the CLI prints to out (if any) and
// folds the ones that carry the outcome into res. Anything unparseable is
// still forwarded: a caller streaming to a browser must see exactly what the
// CLI said.
//
// A long streaming run prints thousands of lines and rota cares about a
// handful of them, so the loop is written to do as little as possible per
// line: one reusable buffer for the copy, and a look at the event's type
// before deciding whether it is worth decoding at all.
func scanEvents(r io.Reader, out io.Writer, keep bool, cp caps, res *Result) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), cp.eventLine)
	var plain strings.Builder
	var scratch []byte
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if out != nil {
			scratch = append(append(scratch[:0], sc.Bytes()...), '\n')
			if _, err := out.Write(scratch); err != nil {
				return err
			}
			if f, ok := out.(interface{ Flush() }); ok {
				f.Flush()
			}
		}
		if len(line) == 0 {
			continue
		}
		if line[0] != '{' && line[0] != '[' {
			plain.Write(line) // a CLI in text mode
			plain.WriteByte('\n')
			continue
		}
		if keep {
			doc := json.RawMessage(slices.Clone(line))
			// An array line carries the events inside it; absorb keeps
			// those. Anything else is itself one event.
			if line[0] != '[' {
				if len(res.Events) < cp.events {
					res.Events = append(res.Events, doc)
				} else {
					res.Truncated = true
				}
			}
			absorb(doc, keep, cp, res)
			continue
		}
		if interesting(line, res) {
			absorb(line, false, cp, res)
		}
	}
	if res.Result == "" {
		res.Result = strings.TrimSpace(plain.String())
	}
	return sc.Err()
}

// eventsOfInterest are the event types that carry an outcome. Every other
// line — the token deltas that make up the bulk of a stream — is copied to
// the caller and otherwise ignored.
//
// The values are matched anywhere in the line rather than parsed out of a
// "type" field, and that is deliberate. A field's position, the spacing
// around its colon and the nesting of an inner "type" are all a vendor's
// choice, and guessing wrong here does not fail loudly: the outcome event
// is never decoded and the run returns an empty answer with no error. A
// needless unmarshal costs microseconds; a lost result costs the run.
var eventsOfInterest = [][]byte{
	[]byte(`"result"`), []byte(`"item.completed"`), []byte(`"turn.completed"`),
	[]byte(`"turn.failed"`), []byte(`"error"`),
}

// interesting reports whether a line is worth decoding: it names one of the
// outcome types, is an array of events, or carries a session id while the
// result still lacks one.
func interesting(line []byte, res *Result) bool {
	if line[0] == '[' {
		return true
	}
	if res.SessionID == "" && (bytes.Contains(line, sessionKey) || bytes.Contains(line, threadKey)) {
		return true
	}
	for _, want := range eventsOfInterest {
		if bytes.Contains(line, want) {
			return true
		}
	}
	return false
}

var (
	sessionKey = []byte(`"session_id"`)
	threadKey  = []byte(`"thread_id"`)
)

// absorb folds one event into the result. It understands Claude Code's
// `result` event, its buffered array of events, and Codex's thread and turn
// events; anything else is ignored.
func absorb(doc json.RawMessage, keep bool, cp caps, res *Result) {
	var arr []json.RawMessage
	if len(doc) > 0 && doc[0] == '[' && decodeLenient(doc, &arr) == nil {
		// A buffered run prints one array of events. Its elements are the
		// events; the array itself is not one, so it is the elements that
		// are kept.
		if keep {
			// Cap what is kept, never what is read: the terminal result
			// event is the last element, and folding it in must not depend
			// on how chatty the run was before it.
			room := cp.events - len(res.Events)
			if room < 0 {
				room = 0
			}
			if len(arr) > room {
				res.Truncated = true
				res.Events = append(res.Events, arr[:room]...)
			} else {
				res.Events = append(res.Events, arr...)
			}
		}
		for _, e := range arr {
			absorb(e, false, cp, res)
		}
		return
	}
	var e struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
		ThreadID  string `json:"thread_id"`
		// grok names the same things differently, and prints one object
		// rather than a stream of typed events.
		SessionIDCamel string          `json:"sessionId"`
		Text           string          `json:"text"`
		StopReason     string          `json:"stopReason"`
		Result         string          `json:"result"`
		Structured     json.RawMessage `json:"structured_output"`
		IsError        bool            `json:"is_error"`
		NumTurns       int             `json:"num_turns"`
		CostUSD        float64         `json:"total_cost_usd"`
		Usage          json.RawMessage `json:"usage"`
		Item           *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
	}
	if decodeLenient(doc, &e) != nil {
		return
	}
	if e.SessionID == "" {
		e.SessionID = e.SessionIDCamel
	}
	if e.SessionID != "" && res.SessionID == "" {
		res.SessionID = e.SessionID
	}
	// A document with an answer and no type is grok's whole reply.
	if e.Type == "" && e.Text != "" {
		res.Result = e.Text
		res.IsError = e.StopReason == "error"
		if e.NumTurns > 0 {
			res.NumTurns = e.NumTurns
		}
		if e.CostUSD > 0 {
			res.CostUSD = e.CostUSD
		}
		if len(e.Usage) > 0 {
			res.Usage = e.Usage
		}
		return
	}
	if e.ThreadID != "" {
		res.SessionID = e.ThreadID
	}
	switch e.Type {
	case "result": // Claude Code
		res.Result, res.Structured, res.IsError = e.Result, e.Structured, e.IsError
		res.Subtype, res.NumTurns, res.CostUSD, res.Usage = e.Subtype, e.NumTurns, e.CostUSD, e.Usage
	case "item.completed": // Codex
		if e.Item != nil && e.Item.Type == "agent_message" {
			res.Result = e.Item.Text
		}
	case "turn.completed":
		res.Usage = e.Usage
	case "turn.failed", "error":
		res.IsError = true
	}
}

// caps are the output bounds one run works under: Limits values where the
// operator set them, the package defaults where not.
type caps struct {
	buffered  int64
	eventLine int
	stderr    int
	events    int
}

func (l *Limits) caps() caps {
	c := caps{buffered: maxBufferedOutput, eventLine: maxEventLine, stderr: maxStderr, events: maxEvents}
	if l == nil {
		return c
	}
	if l.MaxBufferedOutput > 0 {
		c.buffered = int64(l.MaxBufferedOutput)
	}
	if l.MaxEventLine > 0 {
		c.eventLine = l.MaxEventLine
	}
	if l.MaxStderr > 0 {
		c.stderr = l.MaxStderr
	}
	if l.MaxEvents > 0 {
		c.events = l.MaxEvents
	}
	return c
}

const (
	// maxEventLine bounds one event line. Claude Code emits whole messages
	// on a single line, so this is generous.
	maxEventLine = 8 << 20
	// maxStderr is how much of a CLI's diagnostic output is kept. It is the
	// tail: the end of it is where a failure says why.
	maxStderr = 64 << 10
	// maxEvents bounds what include_events returns, so one talkative run
	// cannot be asked to build a reply larger than memory.
	maxEvents = 20000
)

// tailBuffer keeps the last limit bytes written to it and counts the rest.
type tailBuffer struct {
	limit   int
	buf     []byte
	dropped int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > t.limit {
		t.dropped += len(t.buf) + len(p) - t.limit
		t.buf = append(t.buf[:0], p[len(p)-t.limit:]...)
		return n, nil
	}
	if over := len(t.buf) + len(p) - t.limit; over > 0 {
		t.dropped += over
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) String() string {
	if t.dropped == 0 {
		return string(t.buf)
	}
	return fmt.Sprintf("[%d earlier bytes dropped]\n%s", t.dropped, t.buf)
}

// Check validates a spec against one provider without running anything: the
// same verdicts Run would reach, early enough for a caller to report them
// before spending a slot or a token.
func (s Spec) Check(provider string, lim *Limits) error {
	return s.check(provider, Models(provider), lim)
}

// For fills in what this account already knows and the request did not say.
//
// It returns a copy: a caller holding a Spec it means to reuse should not
// find it quietly rewritten. Apply it before checking, so what is validated
// against the limits is what will actually run.
func (s Spec) For(a *Account) Spec {
	if s.Cwd == "" {
		s.Cwd = a.Cwd
	}
	return s
}

// Resolved reports the model and effort a spec would run with on this
// account, without running it.
//
// A caller that has to say what is about to happen — the first event of a
// stream, a confirmation before spending — needs the answers before the run
// rather than after it, and only the account's own catalog can give them:
// "opus" means a different id on a different plan.
func Resolved(a *Account, home string, s Spec) (model, effort string, err error) {
	model, err = resolveModel(a.Provider, s.Model, ModelsFor(a, home))
	if err != nil {
		return "", "", err
	}
	if effort, err = ResolveEffort(a.Provider, s.Effort); err != nil {
		return "", "", err
	}
	if Flavor(a.Provider) == "kimi" {
		effort = "" // kimi has no effort setting, so it reports none
	}
	return model, effort, nil
}

// CheckFor is Check against one account, so a provider whose models depend
// on the account — codex, whose ChatGPT plan decides — is judged by what
// that account may actually use.
func (s Spec) CheckFor(a *Account, home string, lim *Limits) error {
	return s.check(a.Provider, ModelsFor(a, home), lim)
}

func (s Spec) check(provider string, models []Model, lim *Limits) error {
	// Building a command line stages scratch files — a prompt, a schema —
	// and checking a request must leave nothing behind. The receiver is a
	// copy, and it is the copy planFor records those files on.
	defer s.cleanup()
	if s.TimeoutSeconds < 0 {
		return failf(ErrInvalidRequest, "timeout_seconds must not be negative")
	}
	_, err := s.planFor(provider, models, lim)
	return err
}
