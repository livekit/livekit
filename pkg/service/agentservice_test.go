package service_test

// func TestAgentService(t *testing.T) {
// 	bus := psrpc.NewLocalMessageBus()
// 	client, err := rpc.NewAgentClient(bus)
// 	require.NoError(t, err)
//
// 	svc, err := service.NewAgentService(bus)
// 	require.NoError(t, err)
//
// 	_, err = client.JobRequest(context.Background(), "room", &livekit.Job{Type: livekit.JobType_JT_ROOM})
// 	require.Equal(t, err, psrpc.ErrNoResponse)
//
// 	svc.DrainConnections(0)
// }