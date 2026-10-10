package connect

import (
	gojwt "github.com/golang-jwt/jwt/v5"
)

type ByJwt struct {
	UserId       Id
	NetworkName  string
	NetworkId    Id
	ClientId     Id
	SessionId    *Id
	RootClientId *Id
}

func ParseByJwtUnverified(byJwtStr string) (*ByJwt, error) {
	parser := gojwt.NewParser()
	token, _, err := parser.ParseUnverified(byJwtStr, gojwt.MapClaims{})
	if err != nil {
		return nil, err
	}

	claims := token.Claims.(gojwt.MapClaims)

	byJwt := &ByJwt{}

	readId := func(name string) *Id {
		value, ok := claims[name].(string)
		if !ok {
			return nil
		}
		id, err := ParseId(value)
		if err != nil {
			return nil
		}
		return &id
	}
	if id := readId("user_id"); id != nil {
		byJwt.UserId = *id
	}
	if id := readId("network_id"); id != nil {
		byJwt.NetworkId = *id
	}
	if id := readId("client_id"); id != nil {
		byJwt.ClientId = *id
	}
	byJwt.NetworkName, _ = claims["network_name"].(string)
	byJwt.SessionId = readId("session_id")
	byJwt.RootClientId = readId("root_client_id")

	return byJwt, nil
}
