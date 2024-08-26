/*
 * Copyright 2024 LiveKit, Inc
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

package service

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/twitchtv/twirp"
)

func TestConvertErrToTwirp(t *testing.T) {
	t.Run("nils are handled", func(t *testing.T) {
		require.NoError(t, convertErrToTwirp(nil))
	})
	t.Run("standard errors are passed through", func(t *testing.T) {
		err := errors.New("test")
		cErr := convertErrToTwirp(err)
		require.Error(t, err)
		require.Equal(t, err, cErr)
	})
	t.Run("handles not found", func(t *testing.T) {
		err := ErrRoomNotFound
		cErr := convertErrToTwirp(err)
		require.Error(t, cErr)
		tErr, ok := cErr.(twirp.Error)
		require.True(t, ok)
		require.Equal(t, twirp.NotFound, tErr.Code())
	})
}
