// Copyright © 2017 Microsoft <wastore@microsoft.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package common

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/pkcs12"

	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/confidential"
)

// ApplicationID represents 1st party ApplicationID for AzCopy.
// const ApplicationID = "a45c21f4-7066-40b4-97d8-14f4313c3caa" // 3rd party test ApplicationID for AzCopy.
const ApplicationID = "579a7132-0e58-4d80-b1e1-7a1e2d337859"

// Resource used in azure storage OAuth authentication
const Resource = "https://storage.azure.com"
const MDResource = "https://disk.azure.com/" // There must be a trailing slash-- The service checks explicitly for "https://disk.azure.com/"
const DefaultTenantID = "common"
const DefaultActiveDirectoryEndpoint = "https://login.microsoftonline.com"
const IMDSAPIVersionArcVM = "2019-11-01"
const IMDSAPIVersionAzureVM = "2018-02-01"
const MSIEndpointAzureVM = "http://169.254.169.254/metadata/identity/oauth2/token"
const MSIEndpointArcVM = "http://127.0.0.1:40342/metadata/identity/oauth2/token"

// Refer to https://docs.microsoft.com/en-us/windows/win32/winsock/windows-sockets-error-codes-2 for details
const WSAECONNREFUSED = 10061

var DefaultTokenExpiryWithinThreshold = time.Minute * 10

// UserOAuthTokenManager for token management.
type UserOAuthTokenManager struct {
	oauthClient *http.Client
	credCache   *CredCache

	// Stash the credential info as we delete the environment variable after reading it, and we need to get it multiple times.
	stashedInfo *OAuthTokenInfo
}

// using this from https://github.com/Azure/go-autorest/blob/b3899c1057425994796c92293e931f334af63b4e/autorest/adal/token.go#L1055-L1067
// this struct works with the adal sdks used in clients and azure-cli token requests
type token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`

	// AAD returns expires_in as a string, ADFS returns it as an int
	ExpiresIn json.Number `json:"expires_in"`
	// expires_on can be in two formats, a UTC time stamp or the number of seconds.
	ExpiresOn string      `json:"expires_on"`
	NotBefore json.Number `json:"not_before"`

	Resource string `json:"resource"`
	Type     string `json:"token_type"`
}

// Environment variables injected in the pod
const (
	AzureClientIDEnvVar           = "AZURE_CLIENT_ID"
	AzureTenantIDEnvVar           = "AZURE_TENANT_ID"
	AzureFederatedTokenFileEnvVar = "AZURE_FEDERATED_TOKEN_FILE" // #nosec
	AzureAuthorityHostEnvVar      = "AZURE_AUTHORITY_HOST"
)

// NewUserOAuthTokenManagerInstance creates a token manager instance.
func NewUserOAuthTokenManagerInstance(credCacheOptions CredCacheOptions) *UserOAuthTokenManager {
	return &UserOAuthTokenManager{
		oauthClient: newAzcopyHTTPClient(),
		credCache:   NewCredCache(credCacheOptions),
	}
}

func newAzcopyHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: GlobalProxyLookup,
			// We use Dial instead of DialContext as DialContext has been reported to cause slower performance.
			Dial /*Context*/ : (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 10 * time.Second,
				DualStack: true,
			}).Dial, /*Context*/
			MaxIdleConns:           0, // No limit
			MaxIdleConnsPerHost:    1000,
			IdleConnTimeout:        180 * time.Second,
			TLSHandshakeTimeout:    10 * time.Second,
			ExpectContinueTimeout:  1 * time.Second,
			DisableKeepAlives:      false,
			DisableCompression:     true,
			MaxResponseHeaderBytes: 0,
			// ResponseHeaderTimeout:  time.Duration{},
			// ExpectContinueTimeout:  time.Duration{},
		},
	}
}

// GetTokenInfo gets token info, it follows rule:
// 1. If there is token passed from environment variable(note this is only for testing purpose),
//    use token passed from environment variable.
// 2. Otherwise, try to get token from cache.
// This method either successfully return token, or return error.
func (uotm *UserOAuthTokenManager) GetTokenInfo(ctx context.Context) (*OAuthTokenInfo, error) {
	if uotm.stashedInfo != nil {
		return uotm.stashedInfo, nil
	}

	var tokenInfo *OAuthTokenInfo
	var err error
	if tokenInfo, err = uotm.getTokenInfoFromEnvVar(ctx); err == nil || !IsErrorEnvVarOAuthTokenInfoNotSet(err) {
		// Scenario-Test: unattended testing with oauthTokenInfo set through environment variable
		// Note: Whenever environment variable is set in the context, it will overwrite the cached token info.
		if err != nil { // this is the case when env var exists while get token info failed
			return nil, err
		}
	} else { // Scenario: session mode which get token from cache
		if tokenInfo, err = uotm.getCachedTokenInfo(ctx); err != nil {
			return nil, err
		}
	}

	if tokenInfo == nil || tokenInfo.IsEmpty() {
		return nil, errors.New("invalid state, cannot get valid token info")
	}

	uotm.stashedInfo = tokenInfo

	return tokenInfo, nil
}

// MSILogin tries to get token from MSI, persist indicates whether to cache the token on local disk.
func (uotm *UserOAuthTokenManager) MSILogin(ctx context.Context, identityInfo IdentityInfo, persist bool) (*OAuthTokenInfo, error) {
	if err := identityInfo.Validate(); err != nil {
		return nil, err
	}

	oAuthTokenInfo := &OAuthTokenInfo{
		Identity:     true,
		IdentityInfo: identityInfo,
	}
	token, err := oAuthTokenInfo.GetNewTokenFromMSI(ctx)
	if err != nil {
		return nil, err
	}
	oAuthTokenInfo.Token = *token
	uotm.stashedInfo = oAuthTokenInfo

	if persist {
		err = uotm.credCache.SaveToken(*oAuthTokenInfo)
		if err != nil {
			return nil, err
		}
	}

	return oAuthTokenInfo, nil
}

// secretLoginNoUOTM non-interactively logs in with a client secret.
func secretLoginNoUOTM(tenantID, activeDirectoryEndpoint, secret, applicationID, resource string) (*OAuthTokenInfo, error) {
	if tenantID == "" {
		tenantID = DefaultTenantID
	}

	if activeDirectoryEndpoint == "" {
		activeDirectoryEndpoint = DefaultActiveDirectoryEndpoint
	}

	if applicationID == "" {
		return nil, fmt.Errorf("please supply your OWN application ID")
	}

	oAuthTokenInfo := OAuthTokenInfo{
		Tenant:                  tenantID,
		ActiveDirectoryEndpoint: activeDirectoryEndpoint,
	}

	oauthConfig, err := adal.NewOAuthConfig(activeDirectoryEndpoint, tenantID)
	if err != nil {
		return nil, err
	}

	spt, err := adal.NewServicePrincipalToken(
		*oauthConfig,
		applicationID,
		secret,
		resource,
	)
	if err != nil {
		return nil, err
	}

	err = spt.Refresh()
	if err != nil {
		return nil, err
	}

	// Due to the nature of SPA, no refresh token is given.
	// Thus, no refresh token is copied or needed.
	oAuthTokenInfo.Token = spt.Token()
	oAuthTokenInfo.ApplicationID = applicationID
	oAuthTokenInfo.ServicePrincipalName = true
	oAuthTokenInfo.SPNInfo = SPNInfo{
		Secret:   secret,
		CertPath: "",
	}

	return &oAuthTokenInfo, nil
}

// SecretLogin is a UOTM shell for secretLoginNoUOTM.
func (uotm *UserOAuthTokenManager) SecretLogin(tenantID, activeDirectoryEndpoint, secret, applicationID string, persist bool) (*OAuthTokenInfo, error) {
	oAuthTokenInfo, err := secretLoginNoUOTM(tenantID, activeDirectoryEndpoint, secret, applicationID, Resource)

	if err != nil {
		return nil, err
	}

	uotm.stashedInfo = oAuthTokenInfo
	if persist {
		err = uotm.credCache.SaveToken(*oAuthTokenInfo)
		if err != nil {
			return nil, err
		}
	}

	return oAuthTokenInfo, nil
}

// GetNewTokenFromSecret is a refresh shell for secretLoginNoUOTM
func (credInfo *OAuthTokenInfo) GetNewTokenFromSecret(ctx context.Context) (*adal.Token, error) {
	targetResource := Resource
	if credInfo.Token.Resource != "" && credInfo.Token.Resource != targetResource {
		targetResource = credInfo.Token.Resource
	}

	tokeninfo, err := secretLoginNoUOTM(credInfo.Tenant, credInfo.ActiveDirectoryEndpoint, credInfo.SPNInfo.Secret, credInfo.ApplicationID, targetResource)

	if err != nil {
		return nil, err
	} else {
		return &tokeninfo.Token, nil
	}
}

// Read a potentially encrypted PKCS block
func readPKCSBlock(block *pem.Block, secret []byte, parseFunc func([]byte) (interface{}, error)) (pk interface{}, err error) {
	// Reduce code duplication by baking the parse functions into this
	if x509.IsEncryptedPEMBlock(block) {
		data, err := x509.DecryptPEMBlock(block, secret)

		if err == nil {
			pk, err = parseFunc(data)

			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	} else {
		pk, err = parseFunc(block.Bytes)

		if err != nil {
			return nil, err
		}
	}
	return pk, err
}

func certLoginNoUOTM(tenantID, activeDirectoryEndpoint, certPath, certPass, applicationID, resource string) (*OAuthTokenInfo, error) {
	if tenantID == "" {
		tenantID = DefaultTenantID
	}

	if activeDirectoryEndpoint == "" {
		activeDirectoryEndpoint = DefaultActiveDirectoryEndpoint
	}

	if applicationID == "" {
		return nil, fmt.Errorf("please supply your OWN application ID")
	}

	oAuthTokenInfo := OAuthTokenInfo{
		Tenant:                  tenantID,
		ActiveDirectoryEndpoint: activeDirectoryEndpoint,
	}

	oauthConfig, err := adal.NewOAuthConfig(activeDirectoryEndpoint, tenantID)
	if err != nil {
		return nil, err
	}

	certData, err := ioutil.ReadFile(certPath)
	if err != nil {
		return nil, err
	}

	var pk interface{}
	var cert *x509.Certificate

	if path.Ext(certPath) == ".pfx" || path.Ext(certPath) == ".pkcs12" || path.Ext(certPath) == ".p12" {
		pk, cert, err = pkcs12.Decode(certData, certPass)

		if err != nil {
			return nil, err
		}
	} else if path.Ext(certPath) == ".pem" {
		block, rest := pem.Decode(certData)

		for len(rest) != 0 || pk == nil || cert == nil {
			if block != nil {
				switch block.Type {
				case "ENCRYPTED PRIVATE KEY":
					pk, err = readPKCSBlock(block, []byte(certPass), x509.ParsePKCS8PrivateKey)

					if err != nil {
						return nil, fmt.Errorf("encrypted private key block has invalid format OR your cert password may be incorrect")
					}
				case "RSA PRIVATE KEY":
					pkcs1wrap := func(d []byte) (pk interface{}, err error) {
						return x509.ParsePKCS1PrivateKey(d) // Wrap this so that function signatures agree.
					}

					pk, err = readPKCSBlock(block, []byte(certPass), pkcs1wrap)

					if err != nil {
						return nil, fmt.Errorf("rsa private key block has invalid format OR your cert password may be incorrect")
					}
				case "PRIVATE KEY":
					pk, err = readPKCSBlock(block, []byte(certPass), x509.ParsePKCS8PrivateKey)

					if err != nil {
						return nil, fmt.Errorf("private key block has invalid format")
					}
				case "CERTIFICATE":
					tmpcert, err := x509.ParseCertificate(block.Bytes)

					// Skip this certificate if it's invalid or is a CA cert
					if err == nil && !tmpcert.IsCA {
						cert = tmpcert
					}
				default:
					// Ignore this part of the pem file, don't know what it is.
				}
			} else {
				break
			}

			if len(rest) == 0 {
				break
			}

			block, rest = pem.Decode(rest)
		}

		if pk == nil || cert == nil {
			return nil, fmt.Errorf("could not find the required information (private key & cert) in the supplied .pem file")
		}
	} else {
		return nil, fmt.Errorf("please supply either a .pfx, .pkcs12, .p12, or a .pem file containing a private key and a certificate")
	}

	p, ok := pk.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("only RSA private keys are supported")
	}

	spt, err := adal.NewServicePrincipalTokenFromCertificate(
		*oauthConfig,
		applicationID,
		cert,
		p,
		resource,
	)
	if err != nil {
		return nil, err
	}

	err = spt.Refresh()
	if err != nil {
		return nil, err
	}

	cpfq, _ := filepath.Abs(certPath)

	oAuthTokenInfo.Token = spt.Token()
	oAuthTokenInfo.RefreshToken = oAuthTokenInfo.Token.RefreshToken
	oAuthTokenInfo.ApplicationID = applicationID
	oAuthTokenInfo.ServicePrincipalName = true
	oAuthTokenInfo.SPNInfo = SPNInfo{
		Secret:   certPass,
		CertPath: cpfq,
	}

	return &oAuthTokenInfo, nil
}

// CertLogin non-interactively logs in using a specified certificate, certificate password, and activedirectory endpoint.
func (uotm *UserOAuthTokenManager) CertLogin(tenantID, activeDirectoryEndpoint, certPath, certPass, applicationID string, persist bool) (*OAuthTokenInfo, error) {
	// TODO: Global default cert flag for true non interactive login?
	// (Also could be useful if the user has multiple certificates they want to switch between in the same file.)
	oAuthTokenInfo, err := certLoginNoUOTM(tenantID, activeDirectoryEndpoint, certPath, certPass, applicationID, Resource)
	uotm.stashedInfo = oAuthTokenInfo

	if persist && err == nil {
		err = uotm.credCache.SaveToken(*oAuthTokenInfo)
		if err != nil {
			return nil, err
		}
	}

	return oAuthTokenInfo, err
}

// GetNewTokenFromCert refreshes a token manually from a certificate.
func (credInfo *OAuthTokenInfo) GetNewTokenFromCert(ctx context.Context) (*adal.Token, error) {
	targetResource := Resource
	if credInfo.Token.Resource != "" && credInfo.Token.Resource != targetResource {
		targetResource = credInfo.Token.Resource
	}

	tokeninfo, err := certLoginNoUOTM(credInfo.Tenant, credInfo.ActiveDirectoryEndpoint, credInfo.SPNInfo.CertPath, credInfo.SPNInfo.Secret, credInfo.ApplicationID, targetResource)

	if err != nil {
		return nil, err
	} else {
		return &tokeninfo.Token, nil
	}
}

// UserLogin interactively logins in with specified tenantID and activeDirectoryEndpoint, persist indicates whether to
// cache the token on local disk.
func (uotm *UserOAuthTokenManager) UserLogin(tenantID, activeDirectoryEndpoint string, persist bool) (*OAuthTokenInfo, error) {
	// Use default tenant ID and active directory endpoint, if nothing specified.
	if tenantID == "" {
		tenantID = DefaultTenantID
	}
	if activeDirectoryEndpoint == "" {
		activeDirectoryEndpoint = DefaultActiveDirectoryEndpoint
	}

	// Init OAuth config
	oauthConfig, err := adal.NewOAuthConfig(activeDirectoryEndpoint, tenantID)
	if err != nil {
		return nil, err
	}

	// Acquire the device code
	deviceCode, err := adal.InitiateDeviceAuth(
		uotm.oauthClient,
		*oauthConfig,
		ApplicationID,
		Resource)
	if err != nil {
		return nil, fmt.Errorf("failed to login with tenantID %q, Azure directory endpoint %q, %v",
			tenantID, activeDirectoryEndpoint, err)
	}

	// Display the authentication message
	fmt.Println(*deviceCode.Message + "\n")

	if tenantID == "" || tenantID == "common" {
		fmt.Println("INFO: Logging in under the \"Common\" tenant. This will log the account in under its home tenant.")
		fmt.Println("INFO: If you plan to use AzCopy with a B2B account (where the account's home tenant is separate from the tenant of the target storage account), please sign in under the target tenant with --tenant-id")
	}

	// Wait here until the user is authenticated
	// TODO: check if adal Go SDK has new method which supports context, currently ctrl-C can stop the login in console interactively.
	token, err := adal.WaitForUserCompletion(uotm.oauthClient, deviceCode)
	if err != nil {
		return nil, fmt.Errorf("failed to login with tenantID %q, Azure directory endpoint %q, %v",
			tenantID, activeDirectoryEndpoint, err)
	}

	oAuthTokenInfo := OAuthTokenInfo{
		Token:                   *token,
		Tenant:                  tenantID,
		ActiveDirectoryEndpoint: activeDirectoryEndpoint,
	}
	uotm.stashedInfo = &oAuthTokenInfo

	// to dump for diagnostic purposes:
	// buf, _ := json.Marshal(oAuthTokenInfo)
	// panic("don't check me in. Buf is " + string(buf))

	if persist {
		err = uotm.credCache.SaveToken(oAuthTokenInfo)
		if err != nil {
			return nil, err
		}
	}

	return &oAuthTokenInfo, nil
}

// getCachedTokenInfo get a fresh token from local disk cache.
// If access token is expired, it will refresh the token.
// If refresh token is expired, the method will fail and return failure reason.
// Fresh token is persisted if acces token or refresh token is changed.
func (uotm *UserOAuthTokenManager) getCachedTokenInfo(ctx context.Context) (*OAuthTokenInfo, error) {
	hasToken, err := uotm.credCache.HasCachedToken()
	if err != nil {
		return nil, fmt.Errorf("no cached token found, please log in with azcopy's login command, %v", err)
	}
	if !hasToken {
		return nil, errors.New("no cached token found, please log in with azcopy's login command")
	}

	tokenInfo, err := uotm.credCache.LoadToken()
	if err != nil {
		return nil, fmt.Errorf("get cached token failed, %v", err)
	}

	freshToken, err := tokenInfo.Refresh(ctx)
	if err != nil {
		return nil, fmt.Errorf("get cached token failed to ensure token fresh, please log in with azcopy's login command again, %v", err)
	}

	// Update token cache, if token is updated.
	if freshToken.AccessToken != tokenInfo.AccessToken || freshToken.RefreshToken != tokenInfo.RefreshToken {
		tokenInfo.Token = *freshToken
		if err := uotm.credCache.SaveToken(*tokenInfo); err != nil {
			return nil, err
		}
	}

	return tokenInfo, nil
}

// HasCachedToken returns if there is cached token in token manager.
func (uotm *UserOAuthTokenManager) HasCachedToken() (bool, error) {
	if uotm.stashedInfo != nil {
		return true, nil
	}

	return uotm.credCache.HasCachedToken()
}

// RemoveCachedToken delete all the cached token.
func (uotm *UserOAuthTokenManager) RemoveCachedToken() error {
	return uotm.credCache.RemoveCachedToken()
}

// ====================================================================================

// EnvVarOAuthTokenInfo passes oauth token info into AzCopy through environment variable.
// Note: this is only used for testing, and not encouraged to be used in production environments.
const EnvVarOAuthTokenInfo = "AZCOPY_OAUTH_TOKEN_INFO"

// ErrorCodeEnvVarOAuthTokenInfoNotSet defines error code when environment variable AZCOPY_OAUTH_TOKEN_INFO is not set.
const ErrorCodeEnvVarOAuthTokenInfoNotSet = "environment variable AZCOPY_OAUTH_TOKEN_INFO is not set"

var stashedEnvOAuthTokenExists = false

// EnvVarOAuthTokenInfoExists verifies if environment variable for OAuthTokenInfo is specified.
// The method returns true if the environment variable is set.
// Note: This is useful for only checking whether the env var exists, please use getTokenInfoFromEnvVar
// directly in the case getting token info is necessary.
func EnvVarOAuthTokenInfoExists() bool {
	if lcm.GetEnvironmentVariable(EEnvironmentVariable.OAuthTokenInfo()) == "" && !stashedEnvOAuthTokenExists {
		return false
	}
	stashedEnvOAuthTokenExists = true
	return true
}

// IsErrorEnvVarOAuthTokenInfoNotSet verifies if an error indicates environment variable AZCOPY_OAUTH_TOKEN_INFO is not set.
func IsErrorEnvVarOAuthTokenInfoNotSet(err error) bool {
	if err != nil && strings.Contains(err.Error(), ErrorCodeEnvVarOAuthTokenInfoNotSet) {
		return true
	}
	return false
}

// getTokenInfoFromEnvVar gets token info from environment variable.
func (uotm *UserOAuthTokenManager) getTokenInfoFromEnvVar(ctx context.Context) (*OAuthTokenInfo, error) {
	rawToken := lcm.GetEnvironmentVariable(EEnvironmentVariable.OAuthTokenInfo())
	if rawToken == "" {
		return nil, errors.New(ErrorCodeEnvVarOAuthTokenInfoNotSet)
	}

	// Remove the env var after successfully fetching once,
	// in case of env var is further spreading into child processes unexpectly.
	lcm.ClearEnvironmentVariable(EEnvironmentVariable.OAuthTokenInfo())

	tokenInfo, err := jsonToTokenInfo([]byte(rawToken))
	if err != nil {
		return nil, fmt.Errorf("get token from environment variable failed to unmarshal token, %v", err)
	}

	if tokenInfo.TokenRefreshSource != TokenRefreshSourceTokenStore {
		refreshedToken, err := tokenInfo.Refresh(ctx)
		if err != nil {
			return nil, fmt.Errorf("get token from environment variable failed to ensure token fresh, %v", err)
		}
		tokenInfo.Token = *refreshedToken
	}

	return tokenInfo, nil
}

// ====================================================================================

// TokenRefreshSourceTokenStore indicates enabling azcopy oauth integration through tokenstore.
// Note: This should be only used for internal integrations.
const TokenRefreshSourceTokenStore = "tokenstore"

// OAuthTokenInfo contains info necessary for refresh OAuth credentials.
type OAuthTokenInfo struct {
	adal.Token
	Tenant                  string `json:"_tenant"`
	ActiveDirectoryEndpoint string `json:"_ad_endpoint"`
	TokenRefreshSource      string `json:"_token_refresh_source"`
	ApplicationID           string `json:"_application_id"`
	Identity                bool   `json:"_identity"`
	IdentityInfo            IdentityInfo
	ServicePrincipalName    bool `json:"_spn"`
	SPNInfo                 SPNInfo
	// Note: ClientID should be only used for internal integrations through env var with refresh token.
	// It indicates the Application ID assigned to your app when you registered it with Azure AD.
	// In this case AzCopy refresh token on behalf of caller.
	// For more details, please refer to
	// https://docs.microsoft.com/en-us/azure/active-directory/develop/v1-protocols-oauth-code#refreshing-the-access-tokens
	ClientID string `json:"_client_id"`
}

// IdentityInfo contains info for MSI.
type IdentityInfo struct {
	ClientID string `json:"_identity_client_id"`
	ObjectID string `json:"_identity_object_id"`
	MSIResID string `json:"_identity_msi_res_id"`
}

// SPNInfo contains info for authenticating with Service Principal Names
type SPNInfo struct {
	// Secret is used for two purposes: The certificate secret, and a client secret.
	// The secret is persisted to the JSON file because AAD does not issue a refresh token.
	// Thus, the original secret is needed to refresh.
	Secret   string `json:"_spn_secret"`
	CertPath string `json:"_spn_cert_path"`
}

// Validate validates identity info, at most only one of clientID, objectID or MSI resource ID could be set.
func (identityInfo *IdentityInfo) Validate() error {
	v := make(map[string]bool, 3)
	if identityInfo.ClientID != "" {
		v[identityInfo.ClientID] = true
	}
	if identityInfo.ObjectID != "" {
		v[identityInfo.ObjectID] = true
	}
	if identityInfo.MSIResID != "" {
		v[identityInfo.MSIResID] = true
	}
	if len(v) > 1 {
		return errors.New("client ID, object ID and MSI resource ID are mutually exclusive")
	}
	return nil
}

// Refresh gets new token with token info.
func (credInfo *OAuthTokenInfo) Refresh(ctx context.Context) (*adal.Token, error) {
	if credInfo.TokenRefreshSource == TokenRefreshSourceTokenStore {
		return credInfo.GetNewTokenFromTokenStore(ctx)
	}

	if credInfo.Identity {
		return credInfo.GetNewTokenFromMSI(ctx)
	}

	if credInfo.ServicePrincipalName {
		if credInfo.SPNInfo.CertPath != "" {
			return credInfo.GetNewTokenFromCert(ctx)
		} else {
			return credInfo.GetNewTokenFromSecret(ctx)
		}
	}

	return credInfo.RefreshTokenWithUserCredential(ctx)
}

var msiTokenHTTPClient = newAzcopyHTTPClient()

// Single instance token store credential cache shared by entire azcopy process.
var tokenStoreCredCache = NewCredCacheInternalIntegration(CredCacheOptions{
	KeyName:     "azcopy/aadtoken/" + strconv.Itoa(os.Getpid()),
	ServiceName: "azcopy",
	AccountName: "aadtoken/" + strconv.Itoa(os.Getpid()),
})

// GetNewTokenFromTokenStore gets token from token store. (Credential Manager in Windows, keyring in Linux and keychain in MacOS.)
// Note: This approach should only be used in internal integrations.
func (credInfo *OAuthTokenInfo) GetNewTokenFromTokenStore(ctx context.Context) (*adal.Token, error) {
	hasToken, err := tokenStoreCredCache.HasCachedToken()
	if err != nil || !hasToken {
		return nil, fmt.Errorf("no cached token found in Token Store Mode(SE), %v", err)
	}

	tokenInfo, err := tokenStoreCredCache.LoadToken()
	if err != nil {
		return nil, fmt.Errorf("get cached token failed in Token Store Mode(SE), %v", err)
	}

	return &(tokenInfo.Token), nil
}

// queryIMDS sends a token request to the IMDS endpoint passed by the caller. This IMDS endpoint will be different for Azure and Arc VMs.
func (credInfo *OAuthTokenInfo) queryIMDS(ctx context.Context, msiEndpoint string, resource string, imdsAPIVersion string) (*http.Request, *http.Response, error) {
	// Prepare request to get token from Azure Instance Metadata Service identity endpoint.
	req, err := http.NewRequest("GET", msiEndpoint, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %v", err)
	}

	params := req.URL.Query()
	params.Set("resource", resource)
	params.Set("api-version", imdsAPIVersion)

	if credInfo.IdentityInfo.ClientID != "" {
		params.Set("client_id", credInfo.IdentityInfo.ClientID)
	}
	if credInfo.IdentityInfo.ObjectID != "" {
		params.Set("object_id", credInfo.IdentityInfo.ObjectID)
	}
	if credInfo.IdentityInfo.MSIResID != "" {
		params.Set("msi_res_id", credInfo.IdentityInfo.MSIResID)
	}

	req.URL.RawQuery = params.Encode()
	req.Header.Set("Metadata", "true")

	// Set context.
	req.WithContext(ctx)
	// In case of some other process (Http Server) listening at 127.0.0.1:40342 , we do not want to wait forever for it to serve request
	msiTokenHTTPClient.Timeout = 10 * time.Second
	// Send request
	resp, err := msiTokenHTTPClient.Do(req)
	// Unset the timeout back
	msiTokenHTTPClient.Timeout = 0
	return req, resp, err
}

// isValidArcResponse checks if the key "Www-Authenticate" is unavailable in the header of an http response
func isValidArcResponse(resp *http.Response) bool {
	wwwAuthenticateExists := false
	if resp != nil && resp.Header != nil {
		// Parameter for validity is whether "Www-Authenticate" exists in the response header
		// "Www-Authenticate" contains the path to the challenge token file for Arc VMs
		_, wwwAuthenticateExists = resp.Header["Www-Authenticate"]
	}

	return wwwAuthenticateExists
}

// fixupTokenJson corrects the value of JSON field "not_before" in the Byte slice from blank to a valid value and returns the corrected Byte slice.

// Dated 15th Sep 2021.
// Token JSON returned by ARC-server endpoint API currently does not set a valid integral value for "not_before" key.
// If the token JSON already has "not_before" correctly set, this will be a no-op.
func fixupTokenJson(bytes []byte) []byte {
	byteSliceToString := string(bytes)
	separatorString := `"not_before":"`
	stringSlice := strings.Split(byteSliceToString, separatorString)

	if stringSlice[1][0] != '"' {
		return bytes
	}

	// If the value of not_before is blank, set to "now - 5 sec" and return the updated slice
	notBeforeTimeInteger := uint64(time.Now().Unix() - 5)
	notBeforeTime := strconv.FormatUint(notBeforeTimeInteger, 10)
	return []byte(stringSlice[0] + separatorString + notBeforeTime + stringSlice[1])
}

// GetNewTokenFromMSI gets token from Azure Instance Metadata Service identity endpoint. It first checks if the VM is registered with Azure Arc. Failing that case, it checks if it is an Azure VM.
// For details, please refer to https://docs.microsoft.com/en-us/azure/active-directory/managed-identities-azure-resources/overview
// Note: The msiTokenHTTPClient timeout is has been reduced from 30 sec to 10 sec as IMDS endpoint is local to the machine.
// Without this change, if some router is configured to not return "ICMP unreachable" then it will take 30 secs to timeout and increase the response time.
// We are additionally checking Arc first, and then Azure VM because Arc endpoint is local so as to further reduce the response time of the Azure VM IMDS endpoint.
func (credInfo *OAuthTokenInfo) GetNewTokenFromMSI(ctx context.Context) (*adal.Token, error) {
	targetResource := Resource
	if credInfo.Token.Resource != "" && credInfo.Token.Resource != targetResource {
		targetResource = credInfo.Token.Resource
	}
	// doTokenRequest was copied from https://github.com/Azure/azure-workload-identity/blob/main/pkg/proxy/proxy.go
	// this makes it possible to use azcopy with Workload Identity, without requiring a proxy container
	token, err := doTokenRequest(ctx,
		os.Getenv(AzureClientIDEnvVar),
		targetResource,
		os.Getenv(AzureTenantIDEnvVar),
		os.Getenv(AzureAuthorityHostEnvVar),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get token: %s", err)
	}
	return &adal.Token{
		AccessToken:  token.AccessToken,
		Resource:     token.Resource,
		Type:         token.Type,
		ExpiresOn:    json.Number(token.ExpiresOn),
		ExpiresIn:    token.ExpiresIn,
		NotBefore:    token.NotBefore,
		RefreshToken: token.RefreshToken,
	}, nil
}

func doTokenRequest(ctx context.Context, clientID, resource, tenantID, authorityHost string) (*token, error) {
	tokenFilePath := os.Getenv(AzureFederatedTokenFileEnvVar)
	cred := confidential.NewCredFromAssertionCallback(
		func(
			context.Context,
			confidential.AssertionRequestOptions,
		) (string, error) {
			return readJWTFromFS(tokenFilePath)
		})

	confidentialClientApp, err := confidential.New(clientID, cred,
		confidential.WithAuthority(fmt.Sprintf("%s%s/oauth2/token", authorityHost, tenantID)))
	if err != nil {
		return nil, fmt.Errorf("failed to create confidential client app: %s", err)
	}

	// ref: https://github.com/AzureAD/microsoft-authentication-library-for-dotnet/issues/747
	// For MSAL (v2.0 endpoint) asking an access token for a resource that accepts a v1.0 access token,
	// Azure AD parses the desired audience from the requested scope by taking everything before the
	// last slash and using it as the resource identifier.
	// For example, if the scope is "https://vault.azure.net/.default", the resource identifier is "https://vault.azure.net".
	// If the scope is "http://database.windows.net//.default", the resource identifier is "http://database.windows.net/".
	scope := resource
	if !strings.HasPrefix(scope, "/.default") {
		scope = scope + "/.default"
	}
	result, err := confidentialClientApp.AcquireTokenByCredential(ctx, []string{scope})
	if err != nil {
		return nil, fmt.Errorf("failed to acquire token: %s", err)
	}

	return &token{
		AccessToken: result.AccessToken,
		Resource:    resource,
		Type:        "Bearer",
		// There is a difference in parsing between the azure sdks and how azure-cli works
		// Using the unix time to be consistent with response from IMDS which works with
		// all the clients.
		ExpiresOn: strconv.FormatInt(result.ExpiresOn.UTC().Unix(), 10),
	}, nil
}

func readJWTFromFS(tokenFilePath string) (string, error) {
	token, err := os.ReadFile(tokenFilePath)
	if err != nil {
		return "", err
	}
	return string(token), nil
}

// RefreshTokenWithUserCredential gets new token with user credential through refresh.
func (credInfo *OAuthTokenInfo) RefreshTokenWithUserCredential(ctx context.Context) (*adal.Token, error) {
	targetResource := Resource
	if credInfo.Token.Resource != "" && credInfo.Token.Resource != targetResource {
		targetResource = credInfo.Token.Resource
	}

	oauthConfig, err := adal.NewOAuthConfig(credInfo.ActiveDirectoryEndpoint, credInfo.Tenant)
	if err != nil {
		return nil, err
	}

	// ClientID in credInfo is optional which is used for internal integration only.
	// Use AzCopy's 1st party applicationID for refresh by default.
	spt, err := adal.NewServicePrincipalTokenFromManualToken(
		*oauthConfig,
		IffString(credInfo.ClientID != "", credInfo.ClientID, ApplicationID),
		targetResource,
		credInfo.Token)
	if err != nil {
		return nil, err
	}

	if err := spt.RefreshWithContext(ctx); err != nil {
		return nil, err
	}

	newToken := spt.Token()
	return &newToken, nil
}

// IsEmpty returns if current OAuthTokenInfo is empty and doesn't contain any useful info.
func (credInfo OAuthTokenInfo) IsEmpty() bool {
	if credInfo.Tenant == "" && credInfo.ActiveDirectoryEndpoint == "" && credInfo.Token.IsZero() && !credInfo.Identity {
		return true
	}

	return false
}

// toJSON converts OAuthTokenInfo to json format.
func (credInfo OAuthTokenInfo) toJSON() ([]byte, error) {
	return json.Marshal(credInfo)
}

// jsonToTokenInfo converts bytes to OAuthTokenInfo
func jsonToTokenInfo(b []byte) (*OAuthTokenInfo, error) {
	var OAuthTokenInfo OAuthTokenInfo
	if err := json.Unmarshal(b, &OAuthTokenInfo); err != nil {
		return nil, err
	}
	return &OAuthTokenInfo, nil
}

// ====================================================================================

// TestOAuthInjection controls variables for OAuth testing injections
type TestOAuthInjection struct {
	DoTokenRefreshInjection bool
	TokenRefreshDuration    time.Duration
}

// GlobalTestOAuthInjection is the global setting for OAuth testing injection control
var GlobalTestOAuthInjection = TestOAuthInjection{
	DoTokenRefreshInjection: false,
	TokenRefreshDuration:    time.Second * 10,
}

// TODO: Add pipeline policy for token refreshing validating.
