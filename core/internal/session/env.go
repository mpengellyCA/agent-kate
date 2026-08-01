package session

import (
	"os"
	"regexp"
	"strings"
	"unicode"
)

// EnvNotStored is what a persisted per-thread Env entry carries in place of a
// credential-shaped VALUE. The key survives — the record still says "this
// thread runs with FIREWORKS_API_KEY set" — and the UI can render the marker as
// "value not stored" rather than showing the thread as having no such variable
// at all. Silently dropping the key would be worse: the human would have no way
// to tell a redacted overlay from an overlay that was never set.
//
// Deliberately unmistakable: no plausible real environment value looks like it,
// so resolveEnvFromProcess cannot mistake a legitimate value for a marker.
const EnvNotStored = "__agentkate_not_stored__"

// envSecretKeyRE matches, as a SUBSTRING anywhere in an upper-cased env-overlay
// key, the words that make a variable credential-shaped. Substring rather than
// whole-word because real provider variables glue them on with and without
// separators: ANTHROPIC_API_KEY, OPENAI_APIKEY, GH_TOKEN, DATABASE_PASSWORD,
// TLS_PRIVATE_KEY_FILE.
//
// KEY subsumes APIKEY / API_KEY / ACCESS_KEY / SECRET_KEY / PRIVATE_KEY;
// TOKEN subsumes REFRESH_TOKEN / ID_TOKEN / ACCESS_TOKEN. REFRESH, PRIVATE and
// SIGNATURE are listed anyway so a variable that carries only half the usual
// name (REFRESH, X_SIGNATURE) is still caught.
//
// Deliberately broad: it fires on MONKEY_BUSINESS as well as ANTHROPIC_API_KEY.
// That is the fail-closed direction — a redacted non-secret costs one
// re-resolution from akcore's own environment at resume, an un-redacted secret
// costs a cleartext credential in a file that is never deleted (threads.json is
// rewritten; the archive is FOREVER).
var envSecretKeyRE = regexp.MustCompile(
	`KEY|TOKEN|SECRET|PASSWORD|PASSWD|PASSPHRASE|CREDENTIAL|BEARER|COOKIE|` +
		`PRIVATE|SIGNATURE|SIGNING|SALT|CERT|SESSION|REFRESH|APIKEY`)

// envSecretWords are credential-shaped names that must match a whole NAME
// COMPONENT, never a substring, because as substrings they hit ordinary
// variables: PAT ⊂ PATH / COMPATIBILITY, AUTH ⊂ GIT_AUTHOR_NAME, PASS ⊂
// PASSTHROUGH, SIG ⊂ SIGTERM_GRACE. Components are the pieces between `_`, `-`,
// `.` and camelCase humps (see envKeyComponents).
var envSecretWords = map[string]bool{
	"PAT": true, "AUTH": true, "OAUTH": true, "AUTHORIZATION": true,
	"PASS": true, "PW": true, "PWD": true, "JWT": true,
	"SIG": true, "HMAC": true, "DSN": true, "NONCE": true,
	"OTP": true, "TOTP": true, "SEED": true, "PEM": true,
}

// envSecretValuePrefixes are value shapes that are unmistakably a credential
// whatever the variable is called — the case the key heuristic structurally
// cannot catch, e.g. `MY_THING=sk-ant-…`. Compared case-INSENSITIVELY except
// where a provider's own casing is the distinguishing feature (AKIA, AIza),
// which costs nothing: no ordinary configuration value starts with these.
var envSecretValuePrefixes = []string{
	"sk-", "sk_", "rk_live_", "sq0atp-", "sq0csp-", // OpenAI/Anthropic/Stripe/Square
	"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-", // GitHub/GitLab
	"xoxb-", "xoxp-", "xoxa-", "xoxs-", "xapp-", // Slack
	"AKIA", "ASIA", // AWS access key ids
	"AIza", "ya29.", // Google API key / OAuth token
	"hf_", "npm_", "dop_v1_", "SG.", "shpat_", "eyJ", // HF/npm/DO/SendGrid/Shopify/JWT
	"bearer ", "basic ", "-----begin ", // header values and PEM blocks
}

// EnvKeyIsSecret reports whether an env-overlay key is credential-shaped and so
// must be persisted as EnvNotStored. Exported so the UI-facing layers can label
// what will not survive a restart.
//
// WHAT IT CATCHES: any key containing one of the words in envSecretKeyRE, and
// any key with a whole component in envSecretWords.
//
// WHAT IT DOES NOT CATCH, honestly: a credential in a variable whose name says
// nothing (`FOO=…`, `X=…`), a run-together all-caps name no separator or hump
// splits (`AUTHHEADER`), and anything in a language whose word for "key" is not
// the English one. EnvValueIsSecret is the second net under those, and it too
// only recognises shapes it has been taught. Neither is a proof — the real
// guarantee is that an overlay VALUE never has to be on disk at all: the marker
// keeps the fact of the variable and re-resolution at launch supplies the value.
func EnvKeyIsSecret(key string) bool {
	upper := strings.ToUpper(strings.TrimSpace(key))
	if upper == "" {
		return false
	}
	if envSecretKeyRE.MatchString(upper) {
		return true
	}
	for _, c := range envKeyComponents(key) {
		if envSecretWords[c] {
			return true
		}
	}
	return false
}

// envKeyComponents splits a variable name into upper-cased components on `_`,
// `-`, `.` and camelCase humps, so `githubPat`, `GITHUB_PAT` and `github.pat`
// all yield a "PAT" component while `PATH` yields only "PATH".
func envKeyComponents(key string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToUpper(string(cur)))
			cur = cur[:0]
		}
	}
	runes := []rune(strings.TrimSpace(key))
	for i, r := range runes {
		switch {
		case r == '_' || r == '-' || r == '.' || r == ' ':
			flush()
		case unicode.IsUpper(r) && i > 0 && unicode.IsLower(runes[i-1]):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

// EnvValueIsSecret reports whether a VALUE is unmistakably a credential
// regardless of what its variable is called. It is the second net under
// EnvKeyIsSecret, for `MY_THING=sk-ant-…`.
//
// Only obvious, provider-issued shapes are listed (see envSecretValuePrefixes).
// A generic entropy test is deliberately NOT used: it would redact build hashes,
// commit ids and base64 config blobs, and every false positive here is a value
// the user set that silently stops surviving a restart. False negatives are
// covered by the key heuristic and, ultimately, by not needing the value on
// disk at all.
func EnvValueIsSecret(value string) bool {
	v := strings.TrimSpace(value)
	if v == "" || v == EnvNotStored {
		return false
	}
	lower := strings.ToLower(v)
	for _, p := range envSecretValuePrefixes {
		// AKIA/ASIA/AIza/SG. carry meaning in their casing; the rest are matched
		// case-insensitively so `Bearer …` and `bearer …` both fire.
		if p != strings.ToLower(p) {
			if strings.HasPrefix(v, p) {
				return true
			}
			continue
		}
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// redactEnvForPersist returns a COPY of env with every credential-shaped value
// replaced by EnvNotStored — credential-shaped by KEY (EnvKeyIsSecret) or by
// VALUE (EnvValueIsSecret), either one being enough. A copy, never an in-place
// edit: the caller's map is the live in-memory record, which must keep the real
// value so the running process can still relaunch the thread with it.
//
// Values already equal to the marker (a record loaded from disk whose variable
// is not set in this process's environment) pass through unchanged.
func redactEnvForPersist(env map[string]string) map[string]string {
	if len(env) == 0 {
		return env
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if EnvKeyIsSecret(k) || EnvValueIsSecret(v) {
			out[k] = EnvNotStored
			continue
		}
		out[k] = v
	}
	return out
}

// RedactEnvForWire is redactEnvForPersist under an exported name, for the
// records that leave this process over IPC rather than going to disk. Same
// rule, same marker: a socket peer is no more entitled to another thread's
// credentials than a file on a shared disk is.
func RedactEnvForWire(env map[string]string) map[string]string {
	return redactEnvForPersist(env)
}

// resolveEnvFromProcess replaces EnvNotStored markers with the value the
// variable has in the environment akcore itself runs in.
//
// This is the same mechanism the third-party provider token already uses: the
// Record stores ProviderEnvVar and never the token, and buildEnv resolves the
// token from that variable at launch (internal/agent/provider.go). An overlay
// key is its own variable name, so the lookup needs nothing extra.
//
// A marker with no value in the environment stays a marker — it is NOT dropped.
// LaunchEnv is what keeps it out of a child process; keeping it here preserves
// the "set, but not stored" fact for the UI across restarts.
func resolveEnvFromProcess(env map[string]string) map[string]string {
	if len(env) == 0 {
		return env
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if v == EnvNotStored {
			if live, ok := os.LookupEnv(k); ok && live != "" {
				out[k] = live
				continue
			}
		}
		out[k] = v
	}
	return out
}

// LaunchEnv filters an overlay down to what may be handed to a child process:
// entries still holding EnvNotStored are dropped.
//
// FAIL CLOSED: an unresolved marker means "we do not know this credential".
// Passing the marker string through as a value would set the variable to
// literal garbage — a thread that authenticates with "__agentkate_not_stored__"
// instead of failing to authenticate at all, which is the more confusing of the
// two failures. Dropping it leaves whatever the inherited environment holds,
// which is where the value would have come from anyway.
//
// Every path that builds a harness.StartSpec from a persisted Record runs its
// Env through this.
func LaunchEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return env
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if v == EnvNotStored {
			continue
		}
		out[k] = v
	}
	return out
}
