package middleware

import (
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/nelthaarion/breeze"
)

// JWTOptions defines configurable JWT authentication behavior.
type JWTOptions struct {
	AccessSecret       string                                            // Secret key for access tokens
	RefreshSecret      string                                            // Secret key for refresh tokens
	SigningMethod      jwt.SigningMethod                                 // e.g., jwt.SigningMethodHS256
	TokenLookup        func(ctx *breeze.Context) (string, string, error) // returns (accessToken, refreshToken, error)
	OnUnauthorized     func(ctx *breeze.Context, err error)              // Optional: custom 401 handler
	UserContextKey     string                                            // Key to store claims in ctx store
	RequiredRoles      []string                                          // Optional: roles required to access the route
	ClaimsValidator    func(claims jwt.MapClaims) bool                   // Optional: extra claim validation
	EnableRefreshToken bool                                              // Enable refresh token support
}

// DefaultTokenLookup extracts access token from Authorization header.
//
// FIX: Header keys are stored lowercased by breeze.ParseHTTPRequest
// (request.go, lowerHeaderKey). The previous code looked up
// ctx.Req.Header["Authorization"] (mixed case) which always returned ""
// in production, causing every authenticated request to be rejected as
// "authorization header missing". The bench setup masked this because
// it set the key with the mixed case the middleware expected.
func DefaultTokenLookup(ctx *breeze.Context) (string, string, error) {
	authHeader := ctx.Req.Header["authorization"]
	if authHeader == "" {
		return "", "", fmt.Errorf("authorization header missing")
	}
	parts := strings.Split(authHeader, " ")
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", "", fmt.Errorf("invalid authorization header format")
	}
	return parts[1], "", nil
}

// DefaultUnauthorizedHandler returns 401 Unauthorized.
func DefaultUnauthorizedHandler(ctx *breeze.Context, err error) {
	ctx.Status(401)
	ctx.WriteString("Unauthorized: " + err.Error())
}

// JWTAuthMiddleware returns a JWT authentication middleware.
//
// FIX: The original code stored claims via ctx.SetParam with
// fmt.Sprintf("%v", claims), which produced an unparseable Go map
// representation like "map[exp:...]". Downstream handlers could not
// recover the claims. We now use ctx.Set (typed store) so handlers
// can retrieve claims with a type assertion:
//
//	claims, ok := ctx.Get("user").(jwt.MapClaims)
//
// Performance: ctx.Set lazy-allocates the store map on first use (one
// allocation, capacity 4). This is cheaper than fmt.Sprintf + SetParam
// which allocated a formatted string on every authenticated request.
func JWTAuthMiddleware(opts JWTOptions) breeze.HandlerFunc {
	if opts.SigningMethod == nil {
		opts.SigningMethod = jwt.SigningMethodHS256
	}
	if opts.UserContextKey == "" {
		opts.UserContextKey = "user"
	}
	if opts.TokenLookup == nil {
		opts.TokenLookup = func(ctx *breeze.Context) (string, string, error) {
			tk, _, err := DefaultTokenLookup(ctx)
			return tk, "", err
		}
	}
	if opts.OnUnauthorized == nil {
		opts.OnUnauthorized = DefaultUnauthorizedHandler
	}

	return func(ctx *breeze.Context) {
		accessToken, refreshToken, err := opts.TokenLookup(ctx)
		if err != nil {
			opts.OnUnauthorized(ctx, err)
			return
		}

		claims, valid := validateJWT(accessToken, opts.AccessSecret, opts.SigningMethod)
		if !valid && opts.EnableRefreshToken && refreshToken != "" {
			// Attempt refresh token.
			refreshClaims, ok := validateJWT(refreshToken, opts.RefreshSecret, opts.SigningMethod)
			if ok {
				// Issue new access token.
				newAccessToken, err := GenerateJWT(opts.AccessSecret, jwt.MapClaims{
					"user_id": refreshClaims["user_id"],
					"role":    refreshClaims["role"],
				}, 15*time.Minute, opts.SigningMethod)
				if err == nil {
					ctx.SetHeader("X-New-Access-Token", newAccessToken)
					claims = refreshClaims
					valid = true
				}
			}
		}

		if !valid {
			opts.OnUnauthorized(ctx, fmt.Errorf("invalid token"))
			return
		}

		// Check roles if specified.
		if len(opts.RequiredRoles) > 0 {
			role, _ := claims["role"].(string)
			found := false
			for _, r := range opts.RequiredRoles {
				if r == role {
					found = true
					break
				}
			}
			if !found {
				opts.OnUnauthorized(ctx, fmt.Errorf("insufficient role"))
				return
			}
		}

		// Extra claims validation.
		if opts.ClaimsValidator != nil && !opts.ClaimsValidator(claims) {
			opts.OnUnauthorized(ctx, fmt.Errorf("claims validation failed"))
			return
		}

		// FIX: Store claims as a typed value instead of a fmt.Sprintf string.
		// Downstream handlers retrieve with:
		//   claims, ok := ctx.Get("user").(jwt.MapClaims)
		ctx.Set(opts.UserContextKey, claims)
		ctx.Next()
	}
}

// --- Helpers ---

// validateJWT parses and validates a token string.
func validateJWT(tokenString, secret string, method jwt.SigningMethod) (jwt.MapClaims, bool) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (any, error) {
		if token.Method.Alg() != method.Alg() {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil || !token.Valid {
		return nil, false
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, false
	}
	return claims, true
}

// GenerateJWT generates a new JWT token.
func GenerateJWT(secret string, claims jwt.MapClaims, duration time.Duration, method jwt.SigningMethod) (string, error) {
	if method == nil {
		method = jwt.SigningMethodHS256
	}
	if claims == nil {
		claims = jwt.MapClaims{}
	}
	claims["exp"] = time.Now().Add(duration).Unix()
	token := jwt.NewWithClaims(method, claims)
	return token.SignedString([]byte(secret))
}

// GenerateRefreshToken generates a refresh token.
func GenerateRefreshToken(secret string, claims jwt.MapClaims, duration time.Duration, method jwt.SigningMethod) (string, error) {
	if method == nil {
		method = jwt.SigningMethodHS256
	}
	if claims == nil {
		claims = jwt.MapClaims{}
	}
	claims["exp"] = time.Now().Add(duration).Unix()
	claims["type"] = "refresh"
	token := jwt.NewWithClaims(method, claims)
	return token.SignedString([]byte(secret))
}
