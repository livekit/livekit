package service

import (
	"net/http"
)

func GenBasicAuthMiddleware(username string, password string) (func(http.ResponseWriter, *http.Request, http.HandlerFunc) ) {
	return func(rw http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
		given_username, given_password, ok := r.BasicAuth()
		unauthorized := func() {
			rw.Header().Set("WWW-Authenticate", "Basic realm=\"Protected Area\"")
			rw.WriteHeader(http.StatusUnauthorized)
		}
		if !ok {
			unauthorized()
			return
		} 
	
		if given_username != username {
			unauthorized()
			return
		}
	
		if given_password != password {
			unauthorized()
			return
		}
	
		next(rw, r)
	  }
}