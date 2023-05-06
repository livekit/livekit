package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ethereum/go-ethereum/common/hexutil"
	eth_crypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/twitchtv/twirp"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
)

const (
	authorizationHeader = "Authorization"
	bearerPrefix        = "Bearer "
	accessTokenParam    = "access_token"
)

type grantsKey struct{}

var (
	ErrPermissionDenied          = errors.New("permissions denied")
	ErrMissingAuthorization      = errors.New("invalid authorization header. Must start with " + bearerPrefix)
	ErrInvalidAuthorizationToken = errors.New("invalid authorization token")
)

// authentication middleware
type APIKeyAuthMiddleware struct {
	provider auth.KeyProviderPublicKey
}

func NewAPIKeyAuthMiddleware(provider auth.KeyProviderPublicKey) *APIKeyAuthMiddleware {
	return &APIKeyAuthMiddleware{
		provider: provider,
	}
}

func (m *APIKeyAuthMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	if r.URL != nil && r.URL.Path == "/rtc/validate" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}

	authHeader := r.Header.Get(authorizationHeader)
	var authToken string

	if authHeader != "" {
		if !strings.HasPrefix(authHeader, bearerPrefix) {
			handleError(w, http.StatusUnauthorized, ErrMissingAuthorization)
			return
		}

		authToken = authHeader[len(bearerPrefix):]
	} else {
		// attempt to find from request header
		authToken = r.FormValue(accessTokenParam)
	}

	if authToken != "" {
		v, err := auth.ParseAPIToken(authToken)
		if err != nil {
			handleError(w, http.StatusUnauthorized, ErrInvalidAuthorizationToken)
			return
		}

		fmt.Printf("API KEY is %s\r\n", v.APIKey())
		pk := m.provider.GetPublicKey(v.APIKey())
		if pk == "" {
			handleError(w, http.StatusUnauthorized, errors.New(fmt.Sprintf("wallet %s not exists in contract", v.APIKey())))
			return
		}

		pkb, err := hexutil.Decode(pk)
		if err != nil {
			handleError(w, http.StatusUnauthorized, fmt.Errorf("cannot decode public key %s err %s", pk, err))
			return
		}

		publicKey, err := eth_crypto.UnmarshalPubkey(pkb)
		if err != nil {
			handleError(w, http.StatusUnauthorized, fmt.Errorf("cannot unmarshal public key %s err %s", pk, err))
			return
		}

		grants, err := v.Verify(publicKey)
		if err != nil {
			handleError(w, http.StatusUnauthorized, fmt.Errorf("invalid token: %s, error: %s", authToken, err))
			return
		}

		// set grants in context
		ctx := r.Context()
		r = r.WithContext(context.WithValue(ctx, grantsKey{}, grants))
	}

	next.ServeHTTP(w, r)
}

func GetGrants(ctx context.Context) *auth.ClaimGrants {
	val := ctx.Value(grantsKey{})
	claims, ok := val.(*auth.ClaimGrants)
	if !ok {
		return nil
	}
	return claims
}

func WithGrants(ctx context.Context, grants *auth.ClaimGrants) context.Context {
	return context.WithValue(ctx, grantsKey{}, grants)
}

func SetAuthorizationToken(r *http.Request, token string) {
	r.Header.Set(authorizationHeader, bearerPrefix+token)
}

func EnsureJoinPermission(ctx context.Context) (name livekit.RoomName, err error) {
	claims := GetGrants(ctx)
	if claims == nil || claims.Video == nil {
		err = ErrPermissionDenied
		return
	}

	if claims.Video.RoomJoin {
		name = livekit.RoomName(claims.Video.Room)
	} else {
		err = ErrPermissionDenied
	}
	return
}

func EnsureAdminPermission(ctx context.Context, room livekit.RoomName) error {
	claims := GetGrants(ctx)
	if claims == nil || claims.Video == nil {
		return ErrPermissionDenied
	}

	if !claims.Video.RoomAdmin || room != livekit.RoomName(claims.Video.Room) {
		return ErrPermissionDenied
	}

	return nil
}

func EnsureCreatePermission(ctx context.Context) error {
	claims := GetGrants(ctx)
	if claims == nil || claims.Video == nil || !claims.Video.RoomCreate {
		return ErrPermissionDenied
	}
	return nil
}

func EnsureListPermission(ctx context.Context) error {
	claims := GetGrants(ctx)
	if claims == nil || claims.Video == nil || !claims.Video.RoomList {
		return ErrPermissionDenied
	}
	return nil
}

func EnsureRecordPermission(ctx context.Context) error {
	claims := GetGrants(ctx)
	if claims == nil || claims.Video == nil || !claims.Video.RoomRecord {
		return ErrPermissionDenied
	}
	return nil
}

func EnsureIngressAdminPermission(ctx context.Context) error {
	claims := GetGrants(ctx)
	if claims == nil || claims.Video == nil || !claims.Video.IngressAdmin {
		return ErrPermissionDenied
	}
	return nil
}

// wraps authentication errors around Twirp
func twirpAuthError(err error) error {
	return twirp.NewError(twirp.Unauthenticated, err.Error())
}
