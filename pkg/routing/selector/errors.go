// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package selector

import "errors"

var (
	ErrNoAvailableNodes           = errors.New("could not find any available nodes")
	ErrCurrentRegionNotSet        = errors.New("current region cannot be blank")
	ErrCurrentRegionUnknownLatLon = errors.New("unknown lat and lon for the current region")
	ErrSortByNotSet               = errors.New("sort by option cannot be blank")
	ErrAlgorithmNotSet            = errors.New("node selector algorithm option cannot be blank")
	ErrSortByUnknown              = errors.New("unknown sort by option")
	ErrAlgorithmUnknown           = errors.New("unknown node selector algorithm option")
)
