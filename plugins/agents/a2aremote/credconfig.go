package a2aremote

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"
)

// Configuration of the `credentials:` block, and its validation.
//
// Validation happens HERE, at config load, and is deliberately strict: a
// credentials block naming a key that belongs to a different credential type, a
// `token_env` pointing at an unset variable, a bearer block with no token at
// all — each of those is an operator error that produces a 401 hours later if
// it is allowed through, and a one-line boot failure if it is not. The story
// this file exists to serve is "a misconfigured remote fails loudly at Init,
// not on first delegation".
//
// What is NOT validated here is anything only the remote can answer: whether
// the token is accepted, whether the certificate is trusted, whether the token
// endpoint exists. Those need the network, and this plugin does not touch the
// network at boot.

// Credential config keys.
const (
	cfgKeyCredentials = "credentials"
	cfgKeyCredType    = "type"

	cfgKeyToken       = "token"
	cfgKeyTokenEnv    = "token_env"
	cfgKeyCredHeader  = "header"
	cfgKeyBearerSchem = "scheme"

	cfgKeyClientID        = "client_id"
	cfgKeyClientIDEnv     = "client_id_env"
	cfgKeyClientSecret    = "client_secret"
	cfgKeyClientSecretEnv = "client_secret_env"
	cfgKeyTokenURL        = "token_url"
	cfgKeyScopes          = "scopes"
	cfgKeyAudience        = "audience"
	cfgKeyAuthStyle       = "auth_style"
	cfgKeyRefreshLeeway   = "refresh_leeway"

	cfgKeyCertFile   = "cert_file"
	cfgKeyKeyFile    = "key_file"
	cfgKeyCAFile     = "ca_file"
	cfgKeyServerName = "server_name"
)

// credentialConfig is one remote's resolved credential configuration. Secrets
// in it are already resolved: an env-var indirection is followed at Init so a
// missing variable fails boot rather than the first call.
type credentialConfig struct {
	kind credentialKind

	// Bearer.
	token  string
	header string
	scheme string

	// OAuth2 client credentials.
	clientID      string
	clientSecret  string
	tokenURL      string
	scopes        []string
	audience      string
	authStyle     string
	refreshLeeway time.Duration

	// mTLS.
	certFile   string
	keyFile    string
	caFile     string
	serverName string
}

// LogValue implements slog.LogValuer so a credentialConfig can never carry a
// secret into a log record, even if some future line logs the whole struct. It
// reports the shape — which credential, from which key — and redacts every
// value. This is belt-and-braces on top of the rule that nothing logs it at
// all.
func (c credentialConfig) LogValue() slog.Value {
	attrs := []slog.Attr{slog.String("type", string(c.kind))}
	switch c.kind {
	case credBearer:
		attrs = append(attrs, slog.String("token", redacted))
	case credOAuth2:
		attrs = append(attrs,
			slog.String("client_id", redacted),
			slog.String("client_secret", redacted),
			slog.String("token_url", c.tokenURL))
	case credMTLS:
		attrs = append(attrs,
			slog.String("cert_file", c.certFile),
			slog.String("key_file", c.keyFile))
	}
	return slog.GroupValue(attrs...)
}

// redacted is the stand-in for every secret value.
const redacted = "[redacted]"

// credentialKeysByKind lists which keys belong to which credential type. It
// drives the "this key belongs to a different type" rejection, which is worth
// having because a `cert_file` under a `type: bearer` block is an operator who
// changed their mind halfway and left half a configuration behind — and JSON
// Schema cannot catch it without an unreadable oneOf.
var credentialKeysByKind = map[credentialKind][]string{
	credNone: {},
	credBearer: {
		cfgKeyToken, cfgKeyTokenEnv, cfgKeyCredHeader, cfgKeyBearerSchem,
	},
	credOAuth2: {
		cfgKeyClientID, cfgKeyClientIDEnv, cfgKeyClientSecret, cfgKeyClientSecretEnv,
		cfgKeyTokenURL, cfgKeyScopes, cfgKeyAudience, cfgKeyAuthStyle, cfgKeyRefreshLeeway,
	},
	credMTLS: {
		cfgKeyCertFile, cfgKeyKeyFile, cfgKeyCAFile, cfgKeyServerName,
	},
}

// parseCredentials resolves an agent's `credentials:` block. An absent block
// means credNone — an open endpoint, which is the correct configuration for a
// loopback remote and the one every existing test uses.
func parseCredentials(m map[string]any, where string) (credentialConfig, error) {
	raw, present := m[cfgKeyCredentials]
	if !present || raw == nil {
		return credentialConfig{kind: credNone}, nil
	}
	block, ok := raw.(map[string]any)
	if !ok {
		return credentialConfig{}, fmt.Errorf("%s: %s.%s: want a mapping, got %T",
			pluginID, where, cfgKeyCredentials, raw)
	}
	at := func(key string) string { return where + "." + cfgKeyCredentials + "." + key }

	kindStr := strings.TrimSpace(stringOf(block[cfgKeyCredType]))
	if kindStr == "" {
		return credentialConfig{}, fmt.Errorf("%s: %s is required; want one of %s",
			pluginID, at(cfgKeyCredType), strings.Join(credentialKinds(), ", "))
	}
	kind := credentialKind(strings.ToLower(kindStr))
	if _, known := credentialKeysByKind[kind]; !known {
		return credentialConfig{}, fmt.Errorf("%s: %s: unknown credential type %q; want one of %s",
			pluginID, at(cfgKeyCredType), kindStr, strings.Join(credentialKinds(), ", "))
	}

	cc := credentialConfig{kind: kind}
	if err := rejectForeignKeys(block, kind, at); err != nil {
		return credentialConfig{}, err
	}

	switch kind {
	case credNone:
		return cc, nil

	case credBearer:
		token, err := secretValue(block, cfgKeyToken, cfgKeyTokenEnv, at)
		if err != nil {
			return credentialConfig{}, err
		}
		if token == "" {
			return credentialConfig{}, fmt.Errorf(
				"%s: %s: a bearer credential needs a token; set %s inline or name an environment variable with %s",
				pluginID, at(cfgKeyCredType), cfgKeyToken, cfgKeyTokenEnv)
		}
		cc.token = token
		cc.header = defaultBearerHeader
		if v := strings.TrimSpace(stringOf(block[cfgKeyCredHeader])); v != "" {
			cc.header = v
		}
		// The scheme is the word before the token. An explicit empty string is
		// how an operator says "send the bare token", which a remote wanting an
		// `X-Api-Key` header needs; only an ABSENT key takes the default.
		cc.scheme = defaultBearerScheme
		if v, set := block[cfgKeyBearerSchem]; set && v != nil {
			cc.scheme = strings.TrimSpace(stringOf(v))
		}
		return cc, nil

	case credOAuth2:
		clientID, err := secretValue(block, cfgKeyClientID, cfgKeyClientIDEnv, at)
		if err != nil {
			return credentialConfig{}, err
		}
		if clientID == "" {
			return credentialConfig{}, fmt.Errorf(
				"%s: %s: an oauth2 client-credentials grant needs a client id; set %s inline or name an environment variable with %s",
				pluginID, at(cfgKeyCredType), cfgKeyClientID, cfgKeyClientIDEnv)
		}
		clientSecret, err := secretValue(block, cfgKeyClientSecret, cfgKeyClientSecretEnv, at)
		if err != nil {
			return credentialConfig{}, err
		}
		if clientSecret == "" {
			return credentialConfig{}, fmt.Errorf(
				"%s: %s: an oauth2 client-credentials grant needs a client secret; set %s inline or name an environment variable with %s",
				pluginID, at(cfgKeyCredType), cfgKeyClientSecret, cfgKeyClientSecretEnv)
		}
		cc.clientID = clientID
		cc.clientSecret = clientSecret
		cc.tokenURL = strings.TrimSpace(stringOf(block[cfgKeyTokenURL]))
		cc.audience = strings.TrimSpace(stringOf(block[cfgKeyAudience]))

		if v, set := block[cfgKeyScopes]; set && v != nil {
			scopes, err := stringList(v, at(cfgKeyScopes))
			if err != nil {
				return credentialConfig{}, err
			}
			cc.scopes = scopes
		}
		cc.authStyle = authStyleBasic
		if v, set := block[cfgKeyAuthStyle]; set && v != nil {
			style := strings.ToLower(strings.TrimSpace(stringOf(v)))
			if style != authStyleBasic && style != authStyleBody {
				return credentialConfig{}, fmt.Errorf("%s: %s: unknown auth style %q; want %s or %s",
					pluginID, at(cfgKeyAuthStyle), style, authStyleBasic, authStyleBody)
			}
			cc.authStyle = style
		}
		cc.refreshLeeway = defaultRefreshLeeway
		if v, set := block[cfgKeyRefreshLeeway]; set && v != nil {
			d, err := configDuration(v, at(cfgKeyRefreshLeeway))
			if err != nil {
				return credentialConfig{}, err
			}
			cc.refreshLeeway = d
		}
		return cc, nil

	case credMTLS:
		cc.certFile = strings.TrimSpace(stringOf(block[cfgKeyCertFile]))
		cc.keyFile = strings.TrimSpace(stringOf(block[cfgKeyKeyFile]))
		if cc.certFile == "" || cc.keyFile == "" {
			return credentialConfig{}, fmt.Errorf(
				"%s: %s: a mutual-TLS credential needs both %s and %s",
				pluginID, at(cfgKeyCredType), cfgKeyCertFile, cfgKeyKeyFile)
		}
		cc.caFile = strings.TrimSpace(stringOf(block[cfgKeyCAFile]))
		cc.serverName = strings.TrimSpace(stringOf(block[cfgKeyServerName]))
		return cc, nil
	}
	return cc, nil
}

// rejectForeignKeys refuses a key that belongs to a different credential type.
func rejectForeignKeys(block map[string]any, kind credentialKind, at func(string) string) error {
	allowed := map[string]bool{cfgKeyCredType: true}
	for _, key := range credentialKeysByKind[kind] {
		allowed[key] = true
	}
	// Sorted so the message is stable across runs.
	var foreign []string
	for key := range block {
		if !allowed[key] {
			foreign = append(foreign, key)
		}
	}
	if len(foreign) == 0 {
		return nil
	}
	sort.Strings(foreign)
	return fmt.Errorf("%s: %s: %s does not belong to a %s credential; remove it or change the type",
		pluginID, at(foreign[0]), foreign[0], string(kind))
}

// secretValue resolves a secret from an inline key or the environment variable
// named by its `_env` sibling.
//
// The inline key wins when both are set, matching how every provider in this
// repo resolves api_key against api_key_env. An `_env` naming a variable that
// is unset or empty is an ERROR rather than an empty credential: an operator
// who named a variable meant to use it, and the failure they want is at boot
// with the variable's name, not a 401 later.
//
// No branch of this function puts the VALUE in an error. The variable's name is
// operator-facing information; its contents are not.
func secretValue(block map[string]any, inlineKey, envKey string, at func(string) string) (string, error) {
	if v, set := block[inlineKey]; set && v != nil {
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("%s: %s: want a string, got %T", pluginID, at(inlineKey), v)
		}
		if s = strings.TrimSpace(s); s != "" {
			return s, nil
		}
	}
	envVar := strings.TrimSpace(stringOf(block[envKey]))
	if envVar == "" {
		return "", nil
	}
	value := strings.TrimSpace(os.Getenv(envVar))
	if value == "" {
		return "", fmt.Errorf("%s: %s names environment variable %s, which is unset or empty",
			pluginID, at(envKey), envVar)
	}
	return value, nil
}
