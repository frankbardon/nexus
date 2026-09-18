package googleadc

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/oauth2/google"
)

// CredentialType names the kind of credential ADC resolved. The values match
// the "type" field of a Google credentials JSON file, with TypeMetadata added
// for the keyless case where there is no file at all.
type CredentialType string

const (
	// TypeMetadata is the GCE metadata server — the attached service account of
	// a VM, or the Google service account a GKE Workload Identity binding maps
	// the pod's Kubernetes service account onto. No key material exists.
	TypeMetadata CredentialType = "metadata"
	// TypeServiceAccount is a downloaded service-account key file.
	TypeServiceAccount CredentialType = "service_account"
	// TypeAuthorizedUser is a developer's own credentials, as written by
	// `gcloud auth application-default login`.
	TypeAuthorizedUser CredentialType = "authorized_user"
	// TypeExternalAccount is Workload Identity Federation: an AWS, Azure or
	// OIDC identity exchanged for a Google token.
	TypeExternalAccount CredentialType = "external_account"
	// TypeExternalAccountAuthorizedUser is the user-flow variant of the above.
	TypeExternalAccountAuthorizedUser CredentialType = "external_account_authorized_user"
	// TypeImpersonatedServiceAccount is a credential that authenticates as one
	// identity in order to act as another.
	TypeImpersonatedServiceAccount CredentialType = "impersonated_service_account"
	// TypeGDCHServiceAccount is a Google Distributed Cloud Hosted service
	// account.
	TypeGDCHServiceAccount CredentialType = "gdch_service_account"
)

// supportedTypes maps the credential types this source accepts from a file to
// the library constant that validates them. A file naming anything else is
// rejected by name rather than handed to the library, so the failure says what
// was in the file.
//
// TypeMetadata is absent on purpose: it never comes from a file.
var supportedTypes = map[CredentialType]google.CredentialsType{
	TypeServiceAccount:                google.ServiceAccount,
	TypeAuthorizedUser:                google.AuthorizedUser,
	TypeExternalAccount:               google.ExternalAccount,
	TypeExternalAccountAuthorizedUser: google.ExternalAccountAuthorizedUser,
	TypeImpersonatedServiceAccount:    google.ImpersonatedServiceAccount,
	TypeGDCHServiceAccount:            google.GDCHServiceAccount,
}

// supportedTypeList renders supportedTypes for an error message.
func supportedTypeList() string {
	names := make([]string, 0, len(supportedTypes))
	for t := range supportedTypes {
		names = append(names, string(t))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// credentialsFileShape is the subset of a Google credentials JSON file this
// package reads. It reads only enough to name the type and the acting
// identity; everything else about the file is the library's business, and in
// particular no private key is ever unmarshalled here.
type credentialsFileShape struct {
	Type                           CredentialType `json:"type"`
	ClientEmail                    string         `json:"client_email"`
	ClientID                       string         `json:"client_id"`
	Audience                       string         `json:"audience"`
	ServiceIdentityName            string         `json:"service_identity_name"`
	ServiceAccountImpersonationURL string         `json:"service_account_impersonation_url"`
}

// describe reads the credential type and the acting principal out of a
// credentials JSON file.
//
// The principal is best effort by design. Some credential types name an
// identity outright (a service account's client_email); some name it only
// inside a URL (impersonation); some do not name one at all, and for those the
// most useful thing to report is what the credential federates as. An empty
// principal is a thinner log line, never an error.
func describe(jsonData []byte) (CredentialType, string, error) {
	var f credentialsFileShape
	if err := json.Unmarshal(jsonData, &f); err != nil {
		return "", "", fmt.Errorf("parsing the credentials JSON: %w", err)
	}
	if f.Type == "" {
		return "", "", fmt.Errorf("the credentials JSON has no %q field", "type")
	}

	switch f.Type {
	case TypeServiceAccount:
		return f.Type, f.ClientEmail, nil
	case TypeGDCHServiceAccount:
		if f.ClientEmail != "" {
			return f.Type, f.ClientEmail, nil
		}
		return f.Type, f.ServiceIdentityName, nil
	case TypeAuthorizedUser:
		// A user credentials file carries no email — the refresh token names
		// the user only once redeemed. The OAuth client id is the most
		// specific thing present, and it at least distinguishes one developer
		// login from another.
		return f.Type, f.ClientID, nil
	case TypeImpersonatedServiceAccount, TypeExternalAccount, TypeExternalAccountAuthorizedUser:
		if sa := serviceAccountFromImpersonationURL(f.ServiceAccountImpersonationURL); sa != "" {
			return f.Type, sa, nil
		}
		// Federation without impersonation acts as the workload identity pool
		// principal itself, and the audience is what names it.
		return f.Type, f.Audience, nil
	default:
		return f.Type, "", nil
	}
}

// serviceAccountFromImpersonationURL pulls the impersonated service-account
// email out of an IAM Credentials URL of the form
//
//	https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/sa@p.iam.gserviceaccount.com:generateAccessToken
//
// It returns "" for anything that does not have that shape, including the
// empty string, because the only consumer is a log line.
func serviceAccountFromImpersonationURL(url string) string {
	const marker = "/serviceAccounts/"
	i := strings.LastIndex(url, marker)
	if i < 0 {
		return ""
	}
	rest := url[i+len(marker):]
	if j := strings.IndexByte(rest, ':'); j >= 0 {
		rest = rest[:j]
	}
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
