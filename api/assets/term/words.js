"use strict";

/* How a session is said: its name, its status, how it ended, what it runs
 * on, and its ages. Every part of the page that says one of these says it
 * through here, so the list, the strip and the panel never disagree.
 */

// A time in words, which is what a person reads a terminal's age as.
function termAgo(iso) {
  const t = Date.parse(iso || "");
  if (!t) return "—";
  return termSpan(Math.max(0, termNow() - t)) + " ago";
}
function termSpan(ms) {
  const s = Math.round(ms / 1000);
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h " + Math.floor((s % 3600) / 60) + "m";
  return Math.floor(s / 86400) + "d " + Math.floor((s % 86400) / 3600) + "h";
}

// termSharedNote is what a shared terminal is, in the words the list and the
// panel both use: where it came from, and whether anybody here may type.
const termSharedNote = t =>
  "shared from a local terminal" + (t.mode === "watch" ? ", to watch" : "");

// termName is what a session is called: the name it was given, else its
// label, else its id.
function termName(t) {
  return t.name || t.label || t.id;
}

// The words and tones of a session's status. Stopped is said apart, since a
// held session stops and a terminal that is not held ends.
const TERM_STATUS = {
  running: { word: "running", tone: "ok" },
  needs_you: { word: "needs you", tone: "warn" },
  quiet: { word: "quiet", tone: "muted" },
};

// termStatusOf is a session's status as {key, word, tone}: the key is what
// the server says, the word is what a person reads, and the tone is the
// palette's.
function termStatusOf(t) {
  if (t.ended || t.status === "stopped") {
    return { key: "stopped", word: t.held ? "stopped" : "ended", tone: "faint" };
  }
  const key = t.status || "running";
  const known = TERM_STATUS[key] || { word: key, tone: "muted" };
  return { key, word: known.word, tone: known.tone };
}

// termEnding is how a session ended, in words.
function termEnding(t) {
  // A shared terminal has a second way of ending, and it has no code: the
  // rota holding it stopped answering.
  if (t.lost) return t.ending || "the sharing process went away";
  if (t.state === "interrupted") return "interrupted";
  if (t.state === "broken") return "its record cannot be read";
  return `ended, code ${t.exit}`;
}

const TERM_CREDENTIAL = { login: "login", long: "long token", apikey: "API key" };

// termCredential is what the account behind a session launches on: the
// session's own word when its description carries one, else what the
// account says of itself — an API key, a long token still in date, or a
// login. A session with no account, a shell, has none.
function termCredential(t, accounts) {
  if (!t || !t.account) return "";
  const own = TERM_CREDENTIAL[t.account.credential];
  if (own) return own;
  const a = (accounts || []).find(x => x.id === t.account.id);
  if (a && a.credential === "apikey") return "API key";
  if (a && a.long_until && Date.parse(a.long_until) > termNow()) return "long token";
  return "login";
}

// termWaitingWord is what a session waits on its person for, in words.
function termWaitingWord(w) {
  if (w === "permission") return "a permission";
  if (w === "question") return "a question";
  return w || "";
}

// termBase is the last element of a path, either way its slashes lean.
function termBase(path) {
  const parts = String(path || "").split(/[\\/]+/).filter(Boolean);
  return parts.length ? parts[parts.length - 1] : "";
}
