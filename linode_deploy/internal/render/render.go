package render

import (
	"fmt"
	"sort"
	"strings"

	"rtk_account_manager/linode_deploy/internal/manifest"
	"rtk_account_manager/linode_deploy/internal/secrets"
)

type Options struct {
	SkipOIDC      bool
	EnableWorkers bool
}

func RuntimeEnv(m manifest.Manifest, vals secrets.Values, opts Options) (string, string) {
	dbPass := vals.Get("ACCOUNT_MANAGER_DB_PASSWORD")
	accessSecret := vals.Get("JWT_ACCESS_SECRET")
	refreshSecret := vals.Get("JWT_REFRESH_SECRET")
	oidcSecret := vals.Get("OIDC_CLIENT_SECRET")
	dsn := fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable", m.Deploy.DatabaseRole, dbPass, m.Infra().PrivateIP, m.Deploy.DatabaseName)
	lines := []string{
		"DATABASE_URL=" + dsn,
		"JWT_ACCESS_SECRET=" + accessSecret,
		"JWT_REFRESH_SECRET=" + refreshSecret,
		"JWT_SIGNER_PROVIDER=" + vals.Get("JWT_SIGNER_PROVIDER"),
		"JWT_ACCESS_PRIVATE_KEY_PATH=" + vals.Get("JWT_ACCESS_PRIVATE_KEY_PATH"),
		"JWT_ACCESS_PUBLIC_KEY_PATH=" + vals.Get("JWT_ACCESS_PUBLIC_KEY_PATH"),
		"JWT_REFRESH_PRIVATE_KEY_PATH=" + vals.Get("JWT_REFRESH_PRIVATE_KEY_PATH"),
		"JWT_REFRESH_PUBLIC_KEY_PATH=" + vals.Get("JWT_REFRESH_PUBLIC_KEY_PATH"),
		"JWT_ACCESS_PKCS11_MODULE_PATH=" + vals.Get("JWT_ACCESS_PKCS11_MODULE_PATH"),
		"JWT_ACCESS_PKCS11_TOKEN_LABEL=" + vals.Get("JWT_ACCESS_PKCS11_TOKEN_LABEL"),
		"JWT_ACCESS_PKCS11_SLOT_ID=" + vals.Get("JWT_ACCESS_PKCS11_SLOT_ID"),
		"JWT_ACCESS_PKCS11_PIN=" + vals.Get("JWT_ACCESS_PKCS11_PIN"),
		"JWT_ACCESS_PKCS11_KEY_LABEL=" + vals.Get("JWT_ACCESS_PKCS11_KEY_LABEL"),
		"JWT_REFRESH_PKCS11_MODULE_PATH=" + vals.Get("JWT_REFRESH_PKCS11_MODULE_PATH"),
		"JWT_REFRESH_PKCS11_TOKEN_LABEL=" + vals.Get("JWT_REFRESH_PKCS11_TOKEN_LABEL"),
		"JWT_REFRESH_PKCS11_SLOT_ID=" + vals.Get("JWT_REFRESH_PKCS11_SLOT_ID"),
		"JWT_REFRESH_PKCS11_PIN=" + vals.Get("JWT_REFRESH_PKCS11_PIN"),
		"JWT_REFRESH_PKCS11_KEY_LABEL=" + vals.Get("JWT_REFRESH_PKCS11_KEY_LABEL"),
		fmt.Sprintf("PORT=%d", m.Deploy.APIPort),
		"ACCESS_TOKEN_TTL=15m",
		"REFRESH_TOKEN_TTL=720h",
		"AUTH_TOKEN_DELIVERY=log",
		"EMAIL_VERIFICATION_TTL=30m",
		"PASSWORD_RESET_TTL=30m",
		"OTP_RESEND_INTERVAL=60s",
		"OTP_MAX_ATTEMPTS=5",
		"SIGNUP_CAPTCHA_REQUIRED=false",
		"SIGNUP_DISPOSABLE_DOMAINS=",
		"SMTP_HOST=" + vals.Get("SMTP_HOST"),
		"SMTP_PORT=587",
		"SMTP_USERNAME=" + vals.Get("SMTP_USERNAME"),
		"SMTP_PASSWORD=" + vals.Get("SMTP_PASSWORD"),
		"SMTP_FROM=" + vals.Get("SMTP_FROM"),
		"ACCOUNT_MANAGER_INTERNAL_AUTH_TOKEN=" + vals.Get("ACCOUNT_MANAGER_INTERNAL_AUTH_TOKEN"),
	}
	lines = append(lines, crossServiceLines(m, vals, opts)...)
	if opts.SkipOIDC {
		lines = append(lines, "OIDC_ENABLED=false")
	} else {
		lines = append(lines,
			"OIDC_ENABLED=true",
			"OIDC_PROVIDER_ID=keycloak",
			"OIDC_PROVIDER_NAME=Keycloak",
			"OIDC_ISSUER_URL="+vals.Get("OIDC_ISSUER_URL"),
			"OIDC_CLIENT_ID="+vals.Get("OIDC_CLIENT_ID"),
			"OIDC_CLIENT_SECRET="+oidcSecret,
			"OIDC_REDIRECT_URL="+m.Deploy.PublicBaseURL+"/v1/auth/oidc/keycloak/callback",
			"OIDC_SCOPES=openid email profile",
			"OIDC_AUTO_LINK_EMAIL=false",
		)
	}
	env := strings.Join(lines, "\n") + "\n"
	reportLines := append([]string{}, lines...)
	for i, line := range reportLines {
		k, _, _ := strings.Cut(line, "=")
		if secretKey(k) {
			reportLines[i] = k + "=<redacted>"
		}
	}
	sort.Strings(reportLines)
	return env, strings.Join(reportLines, "\n") + "\n"
}

func crossServiceLines(m manifest.Manifest, vals secrets.Values, opts Options) []string {
	lines := []string{
		"ACCOUNT_VIDEO_COMMANDS_STREAM=account.video.commands",
		"VIDEO_ACCOUNT_EVENTS_STREAM=video.account.events",
		"CROSS_SERVICE_CONSUMER_GROUP=rtk_account_manager",
		"CROSS_SERVICE_MAX_ATTEMPTS=5",
		"CROSS_SERVICE_POLL_INTERVAL=5s",
		"AZURE_EVENTHUB_CONNECTION_STRING=" + vals.Get("AZURE_EVENTHUB_CONNECTION_STRING"),
		"AZURE_EVENTHUB_CHECKPOINT_FILE=" + m.Deploy.StateDir + "/azure_eventhubs_checkpoint.json",
	}
	if !opts.EnableWorkers {
		return append([]string{"CROSS_SERVICE_BROKER=log"}, lines...)
	}
	return append([]string{"CROSS_SERVICE_BROKER=log"}, lines...)
}

func secretKey(key string) bool {
	key = strings.ToUpper(key)
	return strings.Contains(key, "PASSWORD") ||
		strings.Contains(key, "PIN") ||
		strings.Contains(key, "SECRET") ||
		strings.Contains(key, "TOKEN") ||
		key == "DATABASE_URL" ||
		strings.Contains(key, "CONNECTION_STRING")
}
