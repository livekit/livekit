package videolayerselector

import (
	"github.com/livekit/protocol/logger"
)

type Null struct {
	*Base
}

func NewNull(logger logger.Logger) *Null {
	return &Null{
		Base: NewBase(logger),
	}
}
