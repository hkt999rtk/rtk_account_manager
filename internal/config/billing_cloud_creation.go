package config

import (
	"fmt"
	"rtk_account_manager/internal/billingbootstrap"
)

func validateBillingCloudCreationConfig(cfg Config) error {
	if cfg.BillingCloudCreationBaseURL == "" && cfg.BillingCloudCreationToken == "" {
		return nil
	}
	if _, err := billingbootstrap.New(billingbootstrap.Config{BaseURL: cfg.BillingCloudCreationBaseURL, Token: cfg.BillingCloudCreationToken}); err != nil {
		return fmt.Errorf("Billing cloud creation requires a trusted origin and dedicated token together")
	}
	for _, secret := range []string{cfg.AccessSecret, cfg.RefreshSecret, cfg.InternalAuthToken, cfg.FactoryProductionJWTSecret, cfg.FactoryEnrollmentToken, cfg.BillingHandoffToken, cfg.VideoCloudLifecycleToken, cfg.SendMailHTTPBearerToken, cfg.EmailOutboxEncryptionKey, cfg.OIDCClientSecret, cfg.JWTAccessPKCS11PIN, cfg.JWTRefreshPKCS11PIN} {
		if secret != "" && secret == cfg.BillingCloudCreationToken {
			return fmt.Errorf("Billing cloud creation must not reuse other credentials")
		}
	}
	return nil
}
