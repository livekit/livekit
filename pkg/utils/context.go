/*
 * Copyright 2023 LiveKit, Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package utils

import (
	"context"

	"github.com/livekit/protocol/logger"
)

type attemptKey struct{}
type loggerKey = struct{}

func ContextWithAttempt(ctx context.Context, attempt int) context.Context {
	return context.WithValue(ctx, attemptKey{}, attempt)
}

func GetAttempt(ctx context.Context) int {
	if attempt, ok := ctx.Value(attemptKey{}).(int); ok {
		return attempt
	}
	return 0
}

func ContextWithLogger(ctx context.Context, logger logger.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

func GetLogger(ctx context.Context) logger.Logger {
	if l, ok := ctx.Value(loggerKey{}).(logger.Logger); ok {
		return l
	}
	return logger.GetLogger()
}
