//go:build windows

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

package prometheus

import "github.com/mackerelio/go-osstat/loadavg"

func getLoadAvg() (*loadavg.Stats, error) {
	return &loadavg.Stats{}, nil
}

func getCPUStats() (cpuLoad float32, numCPUs uint32, err error) {
	return 1, 1, nil
}
