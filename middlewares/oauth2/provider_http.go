package oauth2

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// tokenResponse is the standard RFC 6749 token endpoint response. Providers
// share this shape; provider-specific quirks are handled in their own files.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// toToken converts a raw tokenResponse into the normalized Token, computing the
// absolute expiry from the relative expires_in.
func (tr *tokenResponse) toToken() *Token {
	tok := &Token{
		AccessToken:  tr.AccessToken,
		TokenType:    tr.TokenType,
		RefreshToken: tr.RefreshToken,
		IDToken:      tr.IDToken,
		Scope:        tr.Scope,
	}
	if tr.ExpiresIn > 0 {
		tok.Expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return tok
}

// postForm performs an application/x-www-form-urlencoded POST to a token
// endpoint and decodes the JSON token response. It always sends Accept: JSON
// (required by GitHub, harmless elsewhere) and reuses the config's pooled HTTP
// client. errWrap is the sentinel returned on failure.
func postForm(
	ctx context.Context,
	cfg *Config,
	endpoint string,
	form url.Values,
	errWrap error,
) (*Token, error) {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		endpoint,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errWrap, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errWrap, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("%w: invalid response (status %d)", errWrap, resp.StatusCode)
	}
	if tr.Error != "" {
		return nil, fmt.Errorf("%w: %s %s", errWrap, tr.Error, tr.ErrorDesc)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || tr.AccessToken == "" {
		return nil, fmt.Errorf("%w: status %d", errWrap, resp.StatusCode)
	}
	return tr.toToken(), nil
}

// getJSON performs an authenticated GET (Bearer token) against a user-info
// endpoint and decodes the JSON body into out. extraHeaders lets providers add
// quirks (e.g. GitHub's API version header).
func getJSON(
	ctx context.Context,
	cfg *Config,
	endpoint, accessToken string,
	out any,
	extraHeaders map[string]string,
) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUserInfo, err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUserInfo, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: status %d", ErrUserInfo, resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: %v", ErrUserInfo, err)
	}
	return nil
}

// buildAuthURL assembles a provider authorization URL from common OAuth2
// parameters plus any provider-specific extras. It is used by every driver's
// AuthURL so query encoding stays consistent.
func buildAuthURL(
	base string,
	cfg *Config,
	state, nonce, challenge string,
	extra url.Values,
) string {
	q := url.Values{}
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", cfg.RedirectURL)
	q.Set("response_type", "code")
	q.Set("state", state)
	if len(cfg.Scopes) > 0 {
		q.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	if nonce != "" {
		q.Set("nonce", nonce)
	}
	if challenge != "" {
		q.Set("code_challenge", challenge)
		q.Set("code_challenge_method", "S256")
	}
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	return base + "?" + q.Encode()
}

// exchangeForm builds the standard authorization_code exchange form.
func exchangeForm(cfg *Config, code, verifier string) url.Values {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", cfg.RedirectURL)
	form.Set("client_id", cfg.ClientID)
	form.Set("client_secret", cfg.ClientSecret)
	if verifier != "" {
		form.Set("code_verifier", verifier)
	}
	return form
}

// refreshForm builds the standard refresh_token grant form.
func refreshForm(cfg *Config, refreshToken string) url.Values {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", cfg.ClientID)
	form.Set("client_secret", cfg.ClientSecret)
	return form
}
