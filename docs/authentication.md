# Authentication

LiveKit uses access tokens (JWT) to authenticate clients. A pair of API key and secret key is used to authenticate each API holder.

An access token encapsulate a few pieces of information:

- Grants: what permissions does this token have
- Issuer: which api key issued the token
- Expiration: how long should it be valid for
- Identity: the participant's identity (when joining a room)

Access tokens can be created with `livekit-cli`, or any of the livekit server SDKs
