package model

import (
	"strings"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

const testsVaultResourceName = "tests.yml"

type testsVaultConfig struct {
	Version           int `yaml:"version"`
	EmailVerification struct {
		BypassDomains           []string `yaml:"bypass_domains"`
		SuppressAccountMessages bool     `yaml:"suppress_account_messages"`
	} `yaml:"email_verification"`
	Signup struct {
		Password string `yaml:"password"`
		Phone    struct {
			Number string `yaml:"number"`
		} `yaml:"phone"`
	} `yaml:"signup"`
}

type testAuthPolicy struct {
	BypassVerification      bool
	BypassRateLimits        bool
	AllowPasswordRepair     bool
	ConfiguredPassword      string
	SuppressAccountMessages bool
}

// normalizeTestEmailDomain accepts only ordinary ASCII DNS names. Test auth
// domains should be deliberately boring: wildcards, email addresses, IP
// literals and suffix matching all make a production verification bypass much
// easier to widen accidentally.
func normalizeTestEmailDomain(value string) (string, bool) {
	domain := strings.ToLower(strings.TrimSpace(value))
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" || 253 < len(domain) || strings.ContainsAny(domain, "@*[]:/") {
		return "", false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || 63 < len(label) || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, char := range label {
			if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-') {
				return "", false
			}
		}
	}
	return domain, true
}

func loadTestsVaultConfig() (*testsVaultConfig, error) {
	resource, err := server.Vault.SimpleResource(testsVaultResourceName)
	if err != nil {
		return nil, err
	}
	config := &testsVaultConfig{}
	if err := resource.UnmarshalYamlE(config); err != nil {
		return nil, err
	}
	return config, nil
}

// testAuthPolicyForUserAuth returns an empty policy for every configuration
// problem. That is the fail-closed behavior: normal verification and auth
// attempt limits remain required if tests.yml is missing, malformed, versioned
// unexpectedly, or contains an invalid bypass identity. Email matching is by
// exact configured domain; phone matching is against the one normalized signup
// phone fixture.
func testAuthPolicyForUserAuth(userAuth *string) testAuthPolicy {
	normalUserAuth, authType := NormalUserAuthV1(userAuth)
	if normalUserAuth == nil || (authType != UserAuthTypeEmail && authType != UserAuthTypePhone) {
		return testAuthPolicy{}
	}

	config, err := loadTestsVaultConfig()
	if err != nil {
		return testAuthPolicy{}
	}
	if config.Version != 1 {
		glog.Errorf("[auth] refusing tests.yml auth policy with version %d", config.Version)
		return testAuthPolicy{}
	}

	normalDomains := map[string]bool{}
	for _, configuredDomain := range config.EmailVerification.BypassDomains {
		domain, valid := normalizeTestEmailDomain(configuredDomain)
		if !valid {
			glog.Errorf("[auth] refusing tests.yml auth policy with invalid bypass domain")
			return testAuthPolicy{}
		}
		normalDomains[domain] = true
	}

	var normalConfiguredPhone *string
	if strings.TrimSpace(config.Signup.Phone.Number) != "" {
		configuredPhone, configuredType := NormalUserAuthV1(&config.Signup.Phone.Number)
		if configuredPhone == nil || configuredType != UserAuthTypePhone {
			glog.Errorf("[auth] refusing tests.yml auth policy with invalid signup phone")
			return testAuthPolicy{}
		}
		normalConfiguredPhone = configuredPhone
	}

	matched := false
	if authType == UserAuthTypeEmail {
		at := strings.LastIndexByte(*normalUserAuth, '@')
		if at <= 0 || at == len(*normalUserAuth)-1 {
			return testAuthPolicy{}
		}
		emailDomain, valid := normalizeTestEmailDomain((*normalUserAuth)[at+1:])
		if !valid {
			return testAuthPolicy{}
		}
		matched = normalDomains[emailDomain]
	} else if normalConfiguredPhone != nil {
		matched = *normalConfiguredPhone == *normalUserAuth
	}
	if !matched {
		return testAuthPolicy{}
	}
	policy := testAuthPolicy{
		BypassVerification:      true,
		SuppressAccountMessages: config.EmailVerification.SuppressAccountMessages,
	}
	// A fixed phone fixture cannot vary its identity between campaigns. Let it
	// recover from an interrupted or older campaign only when the caller proves
	// possession of the exact password stored beside that phone in tests.yml.
	// Domain-matched email accounts never receive this repair capability.
	if authType == UserAuthTypePhone && config.Signup.Password != "" {
		policy.BypassRateLimits = true
		policy.AllowPasswordRepair = true
		policy.ConfiguredPassword = config.Signup.Password
	}
	return policy
}
