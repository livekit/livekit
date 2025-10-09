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

package service

import (
	"context"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/protocol/livekit"
)

// Temporary structs until protocol is updated
type UpdateClientConfigurationRequest struct {
	Configurations []*ClientConfigurationItemRequest `json:"configurations"`
}

type ClientConfigurationItemRequest struct {
	Match         string `json:"match"`
	Configuration string `json:"configuration"`
	Merge         bool   `json:"merge"`
}

type UpdateClientConfigurationResponse struct {
	Success bool `json:"success"`
}

type GetClientConfigurationRequest struct {
	ClientInfo *livekit.ClientInfo `json:"client_info"`
}

type GetClientConfigurationResponse struct {
	Configuration *livekit.ClientConfiguration `json:"configuration"`
}

type ClientConfigurationService struct {
	roomManager *RoomManager
}

func NewClientConfigurationService(roomManager *RoomManager) *ClientConfigurationService {
	return &ClientConfigurationService{
		roomManager: roomManager,
	}
}

func (s *ClientConfigurationService) UpdateClientConfiguration(ctx context.Context, req *UpdateClientConfigurationRequest) (*UpdateClientConfigurationResponse, error) {
	// Convert request to config format
	configurations := make([]config.ClientConfigurationItem, len(req.Configurations))
	for i, item := range req.Configurations {
		configurations[i] = config.ClientConfigurationItem{
			Match:         item.Match,
			Configuration: item.Configuration,
			Merge:         item.Merge,
		}
	}

	clientConfig := &config.ClientConfigurationConfig{
		Configurations: configurations,
	}

	// Update the room manager's client configuration
	if err := s.roomManager.UpdateClientConfiguration(clientConfig); err != nil {
		return nil, err
	}

	return &UpdateClientConfigurationResponse{
		Success: true,
	}, nil
}

func (s *ClientConfigurationService) GetClientConfiguration(ctx context.Context, req *GetClientConfigurationRequest) (*GetClientConfigurationResponse, error) {
	// Get configuration for the given client info
	config := s.roomManager.clientConfManager.GetConfiguration(req.ClientInfo)
	
	return &GetClientConfigurationResponse{
		Configuration: config,
	}, nil
}
