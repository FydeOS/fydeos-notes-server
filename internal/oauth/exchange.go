// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package oauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
)

const maxAccessTokenLen = 64 << 10

// providerProfile is the identity a provider asserted.
type providerProfile struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	AvatarURL     string
}

// exchangeRequest carries everything the provider exchange needs.
type exchangeRequest struct {
	provider    OAuthProviderConfig
	code        string
	state       statePayload
	stateToken  string
	appleUser   string
	client      *http.Client
	now         time.Time
	callbackURL string
}

func classifyTransportError(err error) error {
	var retrieve *oauth2.RetrieveError
	var urlErr *url.Error
	switch {
	case errors.As(err, &retrieve):
		status := 0
		if retrieve.Response != nil {
			status = retrieve.Response.StatusCode
		}
		return &providerStatusError{status: status}
	case errors.Is(err, ErrProviderUnavailable), errors.As(err, &urlErr):
		return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	default:
		return fmt.Errorf("%w: %v", ErrInvalidOAuthResponse, err)
	}
}

func exchangeProfile(ctx context.Context, req exchangeRequest) (providerProfile, error) {
	p := req.provider
	ctx = oidc.ClientContext(ctx, req.client)
	if p.Name == providerApple && p.ClientSecret == "" {
		secret, err := appleClientSecret(p, req.now)
		if err != nil {
			return providerProfile{}, err
		}
		p.ClientSecret = secret
	}
	var opts []oauth2.AuthCodeOption
	if p.Name == providerOIDC {
		if req.state.PKCEVerifier == nil {
			return providerProfile{}, ErrInvalidCallbackState
		}
		opts = append(opts, oauth2.VerifierOption(*req.state.PKCEVerifier))
	}
	token, err := oauth2Config(p, req.callbackURL).Exchange(ctx, req.code, opts...)
	if err != nil {
		return providerProfile{}, classifyTransportError(err)
	}
	if err := validateAccessToken(token); err != nil {
		return providerProfile{}, err
	}
	switch p.Name {
	case providerGoogle:
		return googleProfile(ctx, req.client, p, token.AccessToken)
	case providerGitHub:
		return githubProfile(ctx, req.client, p, token.AccessToken)
	case providerApple:
		return appleProfile(ctx, req, p, token)
	case providerOIDC:
		return oidcProfile(ctx, req, p, token)
	case providerFydeOS:
		return fydeosProfile(ctx, req.client, p, token.AccessToken)
	}
	return providerProfile{}, ErrUnknownProvider
}

func validateAccessToken(t *oauth2.Token) error {
	at := t.AccessToken
	if strings.TrimSpace(at) == "" || len(at) > maxAccessTokenLen || strings.IndexFunc(at, isControl) >= 0 {
		return ErrInvalidOAuthResponse
	}
	if t.TokenType != "" && !strings.EqualFold(t.TokenType, "bearer") {
		return ErrInvalidOAuthResponse
	}
	return nil
}

// getJSON performs an authenticated GET and decodes a JSON body.
func getJSON(ctx context.Context, client *http.Client, endpoint, accessToken string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := readCapped(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &providerStatusError{status: resp.StatusCode}
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return ErrInvalidOAuthResponse
	}
	return nil
}

func googleProfile(ctx context.Context, client *http.Client, p OAuthProviderConfig, accessToken string) (providerProfile, error) {
	var info struct {
		ID            json.RawMessage `json:"id"`
		Email         string          `json:"email"`
		VerifiedEmail bool            `json:"verified_email"`
		Name          string          `json:"name"`
		Picture       string          `json:"picture"`
	}
	if err := getJSON(ctx, client, p.UserInfoURL, accessToken, &info); err != nil {
		return providerProfile{}, err
	}
	if !info.VerifiedEmail {
		return providerProfile{}, ErrInvalidOAuthResponse
	}
	return providerProfile{Subject: rawIDString(info.ID), Email: info.Email, EmailVerified: true, Name: info.Name, AvatarURL: info.Picture}, nil
}

// rawIDString accepts a JSON string or number identifier.
func rawIDString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

func githubProfile(ctx context.Context, client *http.Client, p OAuthProviderConfig, accessToken string) (providerProfile, error) {
	var user struct {
		ID        json.Number `json:"id"`
		Email     string      `json:"email"`
		Name      string      `json:"name"`
		Login     string      `json:"login"`
		AvatarURL string      `json:"avatar_url"`
	}
	if err := getJSON(ctx, client, p.UserInfoURL, accessToken, &user); err != nil {
		return providerProfile{}, err
	}
	id := user.ID.String()
	if id == "" || strings.Trim(id, "0123456789") != "" {
		return providerProfile{}, ErrInvalidOAuthResponse
	}
	var emails []struct {
		Email    string `json:"email"`
		Verified bool   `json:"verified"`
		Primary  bool   `json:"primary"`
	}
	if err := getJSON(ctx, client, githubEmailsURL(p), accessToken, &emails); err != nil {
		return providerProfile{}, err
	}
	chosen := ""
	for _, e := range emails {
		if e.Verified && user.Email != "" && strings.EqualFold(e.Email, user.Email) {
			chosen = e.Email
			break
		}
	}
	if chosen == "" {
		for _, e := range emails {
			if e.Verified && e.Primary {
				chosen = e.Email
				break
			}
		}
	}
	if chosen == "" {
		for _, e := range emails {
			if e.Verified {
				chosen = e.Email
				break
			}
		}
	}
	if chosen == "" {
		return providerProfile{}, ErrInvalidOAuthResponse
	}
	name := user.Name
	if strings.TrimSpace(name) == "" {
		name = user.Login
	}
	return providerProfile{Subject: id, Email: chosen, EmailVerified: true, Name: name, AvatarURL: user.AvatarURL}, nil
}

func fydeosProfile(ctx context.Context, client *http.Client, p OAuthProviderConfig, accessToken string) (providerProfile, error) {
	var info struct {
		AccountID     *string `json:"accountId"`
		Email         string  `json:"email"`
		EmailVerified *bool   `json:"emailVerified"`
		ImageURL      string  `json:"imageUrl"`
	}
	if err := getJSON(ctx, client, p.UserInfoURL, accessToken, &info); err != nil {
		return providerProfile{}, err
	}
	if info.AccountID == nil {
		return providerProfile{}, ErrInvalidOAuthResponse
	}
	id := *info.AccountID
	if strings.TrimSpace(id) == "" || strings.TrimSpace(id) != id || len(id) > 512 || strings.IndexFunc(id, isControl) >= 0 || strings.TrimSpace(info.Email) == "" {
		return providerProfile{}, ErrInvalidOAuthResponse
	}
	verified := info.EmailVerified == nil || *info.EmailVerified
	return providerProfile{Subject: id, Email: info.Email, EmailVerified: verified, AvatarURL: info.ImageURL}, nil
}

func appleClientSecret(p OAuthProviderConfig, now time.Time) (string, error) {
	pemText := strings.ReplaceAll(p.Args["privateKey"], `\n`, "\n")
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return "", errors.New("invalid Apple private key")
	}
	var key *ecdsa.PrivateKey
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		k, ok := parsed.(*ecdsa.PrivateKey)
		if !ok {
			return "", errors.New("Apple private key is not an EC key")
		}
		key = k
	} else if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		key = k
	} else {
		return "", errors.New("invalid Apple private key")
	}
	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": p.Args["teamId"],
		"sub": p.ClientID,
		"aud": appleIssuer,
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
	})
	token.Header["kid"] = p.Args["keyId"]
	return token.SignedString(key)
}

// verifyIDToken checks an ID token against the provider JWKS.
func verifyIDToken(ctx context.Context, client *http.Client, jwksURL, issuer, clientID string, algs []string, raw string) (map[string]any, error) {
	if raw == "" {
		return nil, ErrInvalidAuthState
	}
	var set jose.JSONWebKeySet
	if err := getJSON(ctx, client, jwksURL, "", &set); err != nil {
		if errors.Is(err, ErrInvalidOAuthCallbackCode) {
			return nil, fmt.Errorf("%w: jwks %v", ErrProviderUnavailable, err)
		}
		if errors.Is(err, ErrProviderUnavailable) {
			return nil, err
		}
		return nil, ErrInvalidAuthState
	}
	keys := make([]crypto.PublicKey, 0, len(set.Keys))
	for _, k := range set.Keys {
		keys = append(keys, k.Public().Key)
	}
	verifier := oidc.NewVerifier(issuer, &oidc.StaticKeySet{PublicKeys: keys}, &oidc.Config{
		ClientID: clientID, SupportedSigningAlgs: algs, SkipIssuerCheck: true,
	})
	token, err := verifier.Verify(ctx, raw)
	if err != nil {
		return nil, ErrInvalidAuthState
	}
	if strings.TrimRight(token.Issuer, "/") != strings.TrimRight(issuer, "/") {
		return nil, ErrInvalidAuthState
	}
	claims := map[string]any{}
	if err := token.Claims(&claims); err != nil {
		return nil, ErrInvalidAuthState
	}
	return claims, nil
}

func idTokenOf(t *oauth2.Token) string {
	s, _ := t.Extra("id_token").(string)
	return s
}

func appleProfile(ctx context.Context, req exchangeRequest, p OAuthProviderConfig, token *oauth2.Token) (providerProfile, error) {
	claims, err := verifyIDToken(ctx, req.client, p.JWKSURL, appleIssuer, p.ClientID, []string{oidc.RS256, oidc.ES256}, idTokenOf(token))
	if err != nil {
		return providerProfile{}, err
	}
	if nonce, _ := claims["nonce"].(string); nonce != req.state.ClientNonce {
		return providerProfile{}, ErrInvalidAuthState
	}
	if verified, ok := parseBoolish(claims["email_verified"]); !ok || !verified {
		return providerProfile{}, ErrInvalidOAuthResponse
	}
	sub, _ := claims["sub"].(string)
	email, _ := claims["email"].(string)
	profile := providerProfile{Subject: sub, Email: email, EmailVerified: true}
	if req.appleUser != "" {
		var user struct {
			Name struct {
				FirstName string `json:"firstName"`
				LastName  string `json:"lastName"`
			} `json:"name"`
		}
		if json.Unmarshal([]byte(req.appleUser), &user) == nil {
			profile.Name = strings.TrimSpace(user.Name.FirstName + " " + user.Name.LastName)
		}
	}
	return profile, nil
}

var oidcAlgs = []string{oidc.RS256, oidc.RS384, oidc.RS512, oidc.ES256, oidc.ES384, oidc.PS256, oidc.PS384, oidc.PS512}

func oidcProfile(ctx context.Context, req exchangeRequest, p OAuthProviderConfig, token *oauth2.Token) (providerProfile, error) {
	claims, err := verifyIDToken(ctx, req.client, p.JWKSURL, p.Issuer, p.ClientID, oidcAlgs, idTokenOf(token))
	if err != nil {
		return providerProfile{}, err
	}
	if nonce, _ := claims["nonce"].(string); nonce != req.stateToken {
		return providerProfile{}, ErrInvalidAuthState
	}
	userinfo := map[string]any{}
	if err := getJSON(ctx, req.client, p.UserInfoURL, token.AccessToken, &userinfo); err != nil {
		return providerProfile{}, err
	}
	idSub, _ := claims["sub"].(string)
	infoSub, _ := userinfo["sub"].(string)
	if infoSub != idSub {
		return providerProfile{}, ErrInvalidOAuthResponse
	}
	claimName := func(arg, def string) string {
		if v := p.Args[arg]; v != "" {
			return v
		}
		return def
	}
	lookup := func(name string) any {
		if v, ok := userinfo[name]; ok && v != nil {
			return v
		}
		return claims[name]
	}
	str := func(v any) string {
		switch t := v.(type) {
		case string:
			return t
		case float64:
			return strconv.FormatFloat(t, 'f', -1, 64)
		}
		return ""
	}
	subject := str(lookup(claimName("claim_id", "sub")))
	email := str(lookup(claimName("claim_email", "email")))
	name := str(lookup(claimName("claim_name", "name")))
	verified, ok := parseBoolish(lookup(claimName("claim_email_verified", "email_verified")))
	if !ok || !verified || strings.TrimSpace(subject) == "" || strings.TrimSpace(email) == "" {
		return providerProfile{}, ErrInvalidOAuthResponse
	}
	return providerProfile{Subject: subject, Email: email, EmailVerified: true, Name: name}, nil
}

func parseBoolish(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(t) {
		case "true", "1", "yes":
			return true, true
		case "false", "0", "no":
			return false, true
		}
	}
	return false, false
}
