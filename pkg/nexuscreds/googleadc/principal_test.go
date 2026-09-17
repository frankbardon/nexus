package googleadc

import (
	"strings"
	"testing"

	"golang.org/x/oauth2/google"
)

func TestDescribe_ServiceAccountNamesTheClientEmail(t *testing.T) {
	got, principal, err := describe([]byte(`{"type":"service_account","client_email":"agent@p.iam.gserviceaccount.com"}`))
	if err != nil {
		t.Fatalf("describe: unexpected error: %v", err)
	}
	if got != TypeServiceAccount {
		t.Errorf("type = %q, want %q", got, TypeServiceAccount)
	}
	if principal != "agent@p.iam.gserviceaccount.com" {
		t.Errorf("principal = %q, want the client_email", principal)
	}
}

func TestDescribe_AuthorizedUserNamesTheClientID(t *testing.T) {
	got, principal, err := describe([]byte(`{"type":"authorized_user","client_id":"764086051850.apps.googleusercontent.com"}`))
	if err != nil {
		t.Fatalf("describe: unexpected error: %v", err)
	}
	if got != TypeAuthorizedUser {
		t.Errorf("type = %q, want %q", got, TypeAuthorizedUser)
	}
	if principal != "764086051850.apps.googleusercontent.com" {
		t.Errorf("principal = %q, want the client_id", principal)
	}
}

func TestDescribe_ImpersonationNamesTheTargetServiceAccount(t *testing.T) {
	body := `{"type":"impersonated_service_account","service_account_impersonation_url":"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/target@p.iam.gserviceaccount.com:generateAccessToken"}`
	got, principal, err := describe([]byte(body))
	if err != nil {
		t.Fatalf("describe: unexpected error: %v", err)
	}
	if got != TypeImpersonatedServiceAccount {
		t.Errorf("type = %q, want %q", got, TypeImpersonatedServiceAccount)
	}
	if principal != "target@p.iam.gserviceaccount.com" {
		t.Errorf("principal = %q, want the impersonated service account", principal)
	}
}

func TestDescribe_ExternalAccountWithImpersonationNamesTheServiceAccount(t *testing.T) {
	body := `{"type":"external_account","audience":"//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/v","service_account_impersonation_url":"https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/fed@p.iam.gserviceaccount.com:generateAccessToken"}`
	got, principal, err := describe([]byte(body))
	if err != nil {
		t.Fatalf("describe: unexpected error: %v", err)
	}
	if got != TypeExternalAccount {
		t.Errorf("type = %q, want %q", got, TypeExternalAccount)
	}
	if principal != "fed@p.iam.gserviceaccount.com" {
		t.Errorf("principal = %q, want the impersonated service account", principal)
	}
}

func TestDescribe_ExternalAccountWithoutImpersonationNamesTheAudience(t *testing.T) {
	audience := "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/v"
	got, principal, err := describe([]byte(`{"type":"external_account","audience":"` + audience + `"}`))
	if err != nil {
		t.Fatalf("describe: unexpected error: %v", err)
	}
	if got != TypeExternalAccount {
		t.Errorf("type = %q, want %q", got, TypeExternalAccount)
	}
	if principal != audience {
		t.Errorf("principal = %q, want the federation audience %q", principal, audience)
	}
}

func TestDescribe_GDCHServiceAccountFallsBackToTheServiceIdentityName(t *testing.T) {
	got, principal, err := describe([]byte(`{"type":"gdch_service_account","service_identity_name":"gdch-agent"}`))
	if err != nil {
		t.Fatalf("describe: unexpected error: %v", err)
	}
	if got != TypeGDCHServiceAccount {
		t.Errorf("type = %q, want %q", got, TypeGDCHServiceAccount)
	}
	if principal != "gdch-agent" {
		t.Errorf("principal = %q, want the service_identity_name", principal)
	}
}

func TestDescribe_UnknownTypeReportsItWithNoPrincipal(t *testing.T) {
	got, principal, err := describe([]byte(`{"type":"magic_beans","client_email":"x@example.com"}`))
	if err != nil {
		t.Fatalf("describe: unexpected error: %v", err)
	}
	if got != CredentialType("magic_beans") {
		t.Errorf("type = %q, want it reported verbatim", got)
	}
	if principal != "" {
		t.Errorf("principal = %q, want the empty string for a type we cannot interpret", principal)
	}
}

func TestDescribe_RejectsJSONWithNoType(t *testing.T) {
	_, _, err := describe([]byte(`{"client_email":"x@example.com"}`))
	if err == nil {
		t.Fatal("describe accepted credentials JSON with no type field")
	}
	if !strings.Contains(err.Error(), "type") {
		t.Errorf("error %q does not name the missing field", err)
	}
}

func TestDescribe_RejectsMalformedJSON(t *testing.T) {
	if _, _, err := describe([]byte(`not json`)); err == nil {
		t.Fatal("describe accepted malformed JSON")
	}
}

func TestServiceAccountFromImpersonationURL_ExtractsTheEmail(t *testing.T) {
	const url = "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/sa@p.iam.gserviceaccount.com:generateAccessToken"
	if got := serviceAccountFromImpersonationURL(url); got != "sa@p.iam.gserviceaccount.com" {
		t.Fatalf("serviceAccountFromImpersonationURL = %q, want the service account email", got)
	}
}

func TestServiceAccountFromImpersonationURL_ReturnsEmptyForAnyOtherShape(t *testing.T) {
	for _, in := range []string{"", "https://example.com/v1/tokens", "not a url"} {
		if got := serviceAccountFromImpersonationURL(in); got != "" {
			t.Errorf("serviceAccountFromImpersonationURL(%q) = %q, want the empty string", in, got)
		}
	}
}

func TestSupportedTypes_CoverEveryCredentialFileTypeTheLibraryKnows(t *testing.T) {
	// If x/oauth2 grows a seventh file type, this fails and the new one gets a
	// deliberate decision rather than an "unsupported credential type" error
	// in production.
	want := map[CredentialType]google.CredentialsType{
		TypeServiceAccount:                google.ServiceAccount,
		TypeAuthorizedUser:                google.AuthorizedUser,
		TypeExternalAccount:               google.ExternalAccount,
		TypeExternalAccountAuthorizedUser: google.ExternalAccountAuthorizedUser,
		TypeImpersonatedServiceAccount:    google.ImpersonatedServiceAccount,
		TypeGDCHServiceAccount:            google.GDCHServiceAccount,
	}
	if len(supportedTypes) != len(want) {
		t.Fatalf("supportedTypes has %d entries, want %d", len(supportedTypes), len(want))
	}
	for k, v := range want {
		if supportedTypes[k] != v {
			t.Errorf("supportedTypes[%q] = %q, want %q", k, supportedTypes[k], v)
		}
	}
	// TypeMetadata never comes from a file and must not be loadable from one.
	if _, ok := supportedTypes[TypeMetadata]; ok {
		t.Error("supportedTypes contains TypeMetadata, which has no file representation")
	}
}

func TestSupportedTypeList_NamesEveryTypeSorted(t *testing.T) {
	got := supportedTypeList()
	want := "authorized_user, external_account, external_account_authorized_user, gdch_service_account, impersonated_service_account, service_account"
	if got != want {
		t.Fatalf("supportedTypeList() = %q, want %q", got, want)
	}
}
