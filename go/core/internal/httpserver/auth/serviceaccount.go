package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedauth "k8s.io/client-go/kubernetes/typed/authentication/v1"

	"github.com/kagent-dev/kagent/go/core/pkg/auth"
)

// ServiceAccountProvider verifies Kubernetes credentials independently of the
// user provider. Unverified claims only select this verifier, never authority.
type ServiceAccountProvider struct {
	users     auth.AuthProvider
	reviews   typedauth.TokenReviewInterface
	authority string
	audience  string
}

func NewServiceAccountProvider(users auth.AuthProvider, reviews typedauth.TokenReviewInterface, authority, audience string) (*ServiceAccountProvider, error) {
	if users == nil || reviews == nil || authority == "" || audience == "" {
		return nil, errors.New("service-account authentication requires provider, reviewer, authority and audience")
	}
	return &ServiceAccountProvider{users: users, reviews: reviews, authority: authority, audience: audience}, nil
}

func (p *ServiceAccountProvider) Authenticate(ctx context.Context, headers http.Header, query url.Values) (auth.Session, error) {
	selected := false
	for _, header := range headers.Values("Authorization") {
		_, token, _ := strings.Cut(header, " ")
		claims, _ := parseJWTPayload(token)
		subject, _ := claims["sub"].(string)
		_, kubernetes := claims["kubernetes.io"]
		selected = selected || kubernetes || strings.HasPrefix(subject, "system:serviceaccount:")
	}
	if !selected {
		return p.users.Authenticate(ctx, headers, query)
	}
	if len(headers.Values("Authorization")) != 1 {
		return nil, ErrUnauthenticated
	}
	scheme, token, ok := strings.Cut(headers.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return nil, ErrUnauthenticated
	}
	principal, err := p.verify(ctx, token)
	if err != nil {
		return nil, err
	}
	source := &serviceAccountTokenSource{provider: p, token: token, principalID: principal.ID}
	return serviceAccountSession{principal: principal, source: source}, nil
}

func (p *ServiceAccountProvider) verify(ctx context.Context, token string) (auth.Principal, error) {
	review, err := p.reviews.Create(ctx, &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{p.audience}}}, metav1.CreateOptions{})
	// Do not return the API response/error: it may contain credential material.
	if err != nil || review == nil {
		return auth.Principal{}, errors.New("service-account credential verification unavailable")
	}
	s := review.Status
	if !s.Authenticated || s.Error != "" || s.User.UID == "" || !strings.HasPrefix(s.User.Username, "system:serviceaccount:") || !slices.Contains(s.Audiences, p.audience) {
		return auth.Principal{}, ErrUnauthenticated
	}
	return auth.Principal{ID: auth.PrincipalID("kubernetes:"+p.authority, s.User.UID), Claims: map[string]any{"username": s.User.Username, "groups": s.User.Groups}}, nil
}

func (p *ServiceAccountProvider) UpstreamAuth(request *http.Request, session auth.Session, destinationSessionID string) error {
	if _, ok := session.(serviceAccountSession); ok {
		return auth.ForwardCredential(request, session)
	}
	return p.users.UpstreamAuth(request, session, destinationSessionID)
}

// ProjectedSession authenticates the scheduler with its inbound token, then binds
// a separately audience-scoped outbound credential to that verified principal.
// Neither the runtime nor downstream services receive the inbound token.
func (p *ServiceAccountProvider) ProjectedSession(ctx context.Context, identityPath, credentialPath, credentialAudience string) (auth.Session, error) {
	if identityPath == "" || credentialPath == "" || credentialAudience == "" || credentialAudience == p.audience {
		return nil, errors.New("scheduler requires separate inbound and outbound credentials and audiences")
	}
	identity := &serviceAccountTokenSource{provider: p, path: identityPath}
	token, err := identity.read()
	if err != nil {
		return nil, err
	}
	principal, err := p.verify(ctx, token)
	if err != nil {
		return nil, err
	}
	outbound := *p
	outbound.audience = credentialAudience
	source := &serviceAccountTokenSource{provider: &outbound, path: credentialPath, principalID: principal.ID, forbiddenAudience: p.audience}
	if _, err := source.Token(ctx); err != nil {
		return nil, err
	}
	return serviceAccountSession{principal: principal, source: source}, nil
}

type serviceAccountSession struct {
	principal auth.Principal
	source    *serviceAccountTokenSource
}

func (s serviceAccountSession) Principal() auth.Principal { return s.principal }

func (s serviceAccountSession) Token(ctx context.Context) (string, error) {
	return s.source.Token(ctx)
}

type serviceAccountTokenSource struct {
	token             string
	provider          *ServiceAccountProvider
	path              string
	principalID       string
	forbiddenAudience string
}

func (s *serviceAccountTokenSource) read() (string, error) {
	if s.path == "" {
		return s.token, nil
	}
	b, err := os.ReadFile(s.path)
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		return "", errors.New("projected invocation credential unavailable")
	}
	return strings.TrimSpace(string(b)), nil
}

func (s *serviceAccountTokenSource) Token(ctx context.Context) (string, error) {
	token, err := s.read()
	if err != nil {
		return "", err
	}
	principal, err := s.provider.verify(ctx, token)
	if err != nil {
		return "", err
	}
	if principal.ID != s.principalID {
		return "", errors.New("projected invocation credential identity changed")
	}
	if s.forbiddenAudience != "" {
		// TokenReview verified this exact token above. Reject multi-audience
		// credentials that would also be usable for inbound Kagent access.
		claims, err := parseJWTPayload(token)
		if err != nil {
			return "", errors.New("invalid outbound credential")
		}
		audiences, err := jwt.MapClaims(claims).GetAudience()
		if err != nil || slices.Contains(audiences, s.forbiddenAudience) {
			return "", errors.New("outbound credential grants inbound Kagent access")
		}
	}
	return token, nil
}
