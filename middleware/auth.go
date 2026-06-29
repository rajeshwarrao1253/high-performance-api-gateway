// Package middleware provides HTTP middleware implementations for the API gateway.
// The auth middleware implements JWT authentication with claims extraction and RBAC.
package middleware

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"

	"github.com/rajeshwarrao1253/high-performance-api-gateway/config"
)

// Claims represents custom JWT claims with RBAC support.
type Claims struct {
	UserID   string   `json:"user_id"`
	Username string   `json:"username"`
	Email    string   `json:"email"`
	Roles    []string `json:"roles"`
	TenantID string   `json:"tenant_id,omitempty"`
	jwt.RegisteredClaims
}

// contextKey is used for storing values in request context.
type contextKey int

const (
	// ClaimsContextKey is the key for JWT claims in request context.
	ClaimsContextKey contextKey = iota
	// UserIDContextKey is the key for user ID in request context.
	UserIDContextKey
)

// AuthMiddleware handles JWT token validation and RBAC authorization.
type AuthMiddleware struct {
	config    config.JWTConfig
	logger    *zap.Logger
	publicKey interface{}
}

// NewAuthMiddleware creates a new authentication middleware.
func NewAuthMiddleware(cfg config.JWTConfig, logger *zap.Logger) *AuthMiddleware {
	am := &AuthMiddleware{
		config: cfg,
		logger: logger.Named("auth"),
	}

	// Load public key if path is provided
	if cfg.PublicKeyPath != "" {
		// In production, load and parse the public key from the file
		// For this implementation, we use HMAC with the secret
		_ = am.publicKey // Placeholder for public key loading
	}

	return am
}

// Handler returns the HTTP handler that performs authentication.
func (am *AuthMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract token from Authorization header
		tokenString := am.extractToken(r)
		if tokenString == "" {
			am.logger.Debug("missing authorization token",
				zap.String("path", r.URL.Path),
				zap.String("remote_addr", r.RemoteAddr),
			)
			am.respondError(w, http.StatusUnauthorized, "missing authorization token")
			return
		}

		// Parse and validate the token
		claims, err := am.validateToken(tokenString)
		if err != nil {
			am.logger.Debug("invalid token",
				zap.String("path", r.URL.Path),
				zap.Error(err),
			)
			am.respondError(w, http.StatusUnauthorized, fmt.Sprintf("invalid token: %s", err.Error()))
			return
		}

		// Check token expiry with refresh threshold
		if claims.ExpiresAt != nil {
			refreshThreshold := am.config.RefreshThreshold
			if refreshThreshold == 0 {
				refreshThreshold = time.Hour
			}
			if time.Until(claims.ExpiresAt.Time) < refreshThreshold {
				w.Header().Set("X-Token-Refresh", "true")
			}
		}

		// Add claims to request context for downstream use
		ctx := context.WithValue(r.Context(), ClaimsContextKey, claims)
		ctx = context.WithValue(ctx, UserIDContextKey, claims.UserID)

		// Log successful authentication
		am.logger.Debug("authentication successful",
			zap.String("user_id", claims.UserID),
			zap.String("username", claims.Username),
			zap.Strings("roles", claims.Roles),
		)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// extractToken extracts the JWT token from the Authorization header.
func (am *AuthMiddleware) extractToken(r *http.Request) string {
	header := r.Header.Get(am.config.TokenHeader)
	if header == "" {
		return ""
	}

	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return ""
	}

	// Check token prefix (Bearer, etc.)
	if !strings.EqualFold(parts[0], am.config.TokenPrefix) {
		return ""
	}

	return parts[1]
}

// validateToken parses and validates the JWT token string.
func (am *AuthMiddleware) validateToken(tokenString string) (*Claims, error) {
	claims := &Claims{}

	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		// Validate signing method
		switch token.Method.(type) {
		case *jwt.SigningMethodHMAC:
			return []byte(am.config.Secret), nil
		case *jwt.SigningMethodRSA:
			if rsaKey, ok := am.publicKey.(*rsa.PublicKey); ok {
				return rsaKey, nil
			}
			return nil, fmt.Errorf("RSA public key not configured")
		case *jwt.SigningMethodECDSA:
			if ecdsaKey, ok := am.publicKey.(*ecdsa.PublicKey); ok {
				return ecdsaKey, nil
			}
			return nil, fmt.Errorf("ECDSA public key not configured")
		default:
			return nil, fmt.Errorf("unsupported signing method: %v", token.Header["alg"])
		}
	}, jwt.WithValidMethods([]string{"HS256", "HS384", "HS512", "RS256", "RS384", "RS512", "ES256", "ES384", "ES512"}))

	if err != nil {
		return nil, fmt.Errorf("token parsing failed: %w", err)
	}

	if !token.Valid {
		return nil, fmt.Errorf("token is invalid")
	}

	// Validate issuer
	if am.config.Issuer != "" && claims.Issuer != am.config.Issuer {
		return nil, fmt.Errorf("invalid issuer: %s", claims.Issuer)
	}

	// Validate audience
	if len(am.config.Audience) > 0 {
		found := false
		for _, aud := range am.config.Audience {
			if claims.VerifyAudience(aud, true) {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("invalid audience")
		}

	}

	// Validate max age
	if am.config.MaxAge > 0 && claims.IssuedAt != nil {
		maxExpiry := claims.IssuedAt.Add(am.config.MaxAge)
		if time.Now().After(maxExpiry) {
			return nil, fmt.Errorf("token exceeds max age")
		}
	}

	return claims, nil
}

// respondError writes a JSON error response to the client.
func (am *AuthMiddleware) respondError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error":   http.StatusText(code),
		"message": message,
		"status":  code,
	})
}

// GetClaims extracts JWT claims from the request context.
// Returns nil if no claims are present.
func GetClaims(ctx context.Context) *Claims {
	claims, ok := ctx.Value(ClaimsContextKey).(*Claims)
	if !ok {
		return nil
	}
	return claims
}

// GetUserID extracts the user ID from the request context.
func GetUserID(ctx context.Context) string {
	userID, ok := ctx.Value(UserIDContextKey).(string)
	if !ok {
		return ""
	}
	return userID
}

// RequireRole creates a middleware that checks if the authenticated user
// has at least one of the required roles.
func RequireRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims := GetClaims(r.Context())
			if claims == nil {
				http.Error(w, `{"error":"unauthorized","message":"authentication required"}`, http.StatusUnauthorized)
				return
			}

			// Check if user has any of the required roles
			for _, requiredRole := range roles {
				for _, userRole := range claims.Roles {
					if strings.EqualFold(requiredRole, userRole) {
						next.ServeHTTP(w, r)
						return
					}
				}
			}

			// Log access denied
			http.Error(w, `{"error":"forbidden","message":"insufficient permissions"}`, http.StatusForbidden)
		})
	}
}

// Optional creates a variant of the auth middleware that allows
// requests without authentication to pass through.
func (am *AuthMiddleware) Optional(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenString := am.extractToken(r)
		if tokenString == "" {
			// No token provided, continue without claims
			next.ServeHTTP(w, r)
			return
		}

		// Validate token if present
		claims, err := am.validateToken(tokenString)
		if err != nil {
			// Invalid token, continue without claims
			next.ServeHTTP(w, r)
			return
		}

		ctx := context.WithValue(r.Context(), ClaimsContextKey, claims)
		ctx = context.WithValue(ctx, UserIDContextKey, claims.UserID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
