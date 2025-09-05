# Changelog

This project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.9.1] - 2025-09-05

### Fixed

-   swap pub/sub track metrics (#3717)
-   Fix bug with SDP rid, clear only overflow. (#3723)
-   Don't check bindState on downtrack.Bind (#3726)
-   Return highest available layer if requested quality is higher than max (#3729)
-   Fix data packet ParticipantIdentity override logic in participant.go (#3735)
-   Fix svc encoding for chrome mobile on iOS (#3751)
-   offer could be nil when migrating. (#3752)
-   fix(deps): update module github.com/livekit/protocol to v1.39.3 (#3733)
-   bounds check layer index (#3768)
-   Do not send leave if nil (to older clients) (#3817)
-   Fix: RingingTimeout was being skipped for transferParticipant (#3831)
-   Handle no codecs in track info (#3859)
-   Fix missed unlock (#3861)
-   Fix timeout handing in StopEgress (#3876)
-   fix: ensure the participant kind is set on refresh tokens (#3881)
-   Do not advertise NACK for RED. (#3889)
-   Do not send both asb-send-time and twcc. (#3890)
-   Prevent race in determining BWE type. (#3891)

### Added

-   Adds Devin to readme so it auto updates DeepWiki weekly (#3699)
-   Allow passing extra attributes to RTC endpoint. (#3693)
-   warn about credentials when used in tokens (#3705)
-   protocol dep for webhook stats buckets (#3706)
-   for real, pick up protocol change for webhooks queue length buckets (#3707)
-   implement observability for room metrics (#3712)
-   e2e reliability for data channel (#3716)
-   Add simulcast support for WHIP. (#3719)
-   Add Id to SDP signalling messages. (#3722)
-   Set and use rid/spatial layer in TrackInfo. (#3724)
-   Add log for dropping out of order reliable message (#3728)
-   chore: set workerid on job creation (#3737)
-   return error when moving egress/agent participant (#3741)
-   SVC with RID -> spatial layer mapping (#3754)
-   feat(cli-flags): add option for cpu profiling (#3765)
-   Enable H265 by default (#3773)
-   Signalling V2 protocol implementation start (#3794)
-   Signal v2: envelope and fragments as wire message format. (#3800)
-   Grouping all signal messages into participant_signal. (#3801)
-   starting signaller interface (#3802)
-   Signal handling interfaces and participant specific HTTP PATCH. (#3804)
-   Split signal segmenter and reassembler. (#3805)
-   Filling out messages unlikely to change in v2. (#3806)
-   Use signalling utils from protocol (#3807)
-   Validation end point for v2 signalling. (#3811)
-   More v2 signalling changes (#3814)
-   Minor tweak to keep RPC type at service level. (#3815)
-   Add country label to edge prom stats. (#3816)
-   HTTP DELETE of participant session (#3819)
-   Get to the point of establishing subscriber peer connection. (#3821)
-   Get to the point of connecting publisher PC and using it for async signalling (#3822)
-   Support join request as proto + base64 encoded query param (#3836)
-   Use wrapped join request to be able to support compressed and uncompressed. (#3838)
-   handle SyncState in join request (#3839)
-   Support per simulcast codec layers. (#3840)
-   Support video layer mode from client and make most of the code mime aware (#3843)
-   Send `participant_connection_aborted` when participant session is closed (#3848)
-   Support G.711 A-law and U-law (#3849)
-   Extract video size from media stream (#3856)
-   update mediatransport util for ice port 3478 (#3877)
-   Single peer connection mode (#3873)
-   handle frame number wrap back in svc (#3885)
-   Use departure timeout from room preset. (#3888)
-   Use `RequestResponse` to report protocol handling errors (#3895)

### Changed

-   Add a trend check before declaring joint queuing region. (#3701)
-   Small changes to add/use helper functions for length checks. (#3704)
-   remove unused ws signal read loop (#3709)
-   Flush stats on close (#3713)
-   Do not require create permission for WHIP participant. (#3715)
-   Create client config manager in room manager constructor. (#3718)
-   Clear rids from default for layers not published. (#3721)
-   Clear rids if not present in SDP. (#3731)
-   Revert clearing RIDs. (#3732)
-   Take ClientInfo from request. (#3738)
-   remove unused code (#3740)
-   reuse compiled client config scripts (#3743)
-   feat(cli): update to urfave/cli/v3 (#3745)
-   move egress roomID load to launcher (#3748)
-   Log previous allocation to see changes. (#3759)
-   Do not need to just clean up receivers. Remove that interface. (#3760)
-   ClearAllReceivers interface is used to pause relay tracks. (#3761)
-   Temporary change: use pre-defined rids (#3767)
-   Revert "Temporary change: use pre-defined rids" (#3769)
-   Log SDP rids to understand the mapping better. (#3770)
-   Limit taking rids from SDP only in WHIP path. (#3771)
-   Set rids for all codecs. (#3772)
-   Return default layer for invalid rid + track info combination. (#3778)
-   Normalize known rids. (#3779)
-   forward agent id to job state (3786)
-   Map ErrNoResponse to ErrRequestTimedOut in StopEgress to avoid returning 503 (#3788)
-   Set participant active when peerconnection connected (#3790)
-   Handle Metadata field from RoomConfig (#3798)
-   [🤖 readme-manager] Update README (#3808)
-   [🤖 readme-manager] Update README (#3809)
-   Rename RTCRest -> WHIP (#3829)
-   Delete v2 signalling by @boks1971 (#3835)
-   Clean up missed v2 pieces (#3837)
-   Update go deps (#3849)
-   Populate SDP cid in track info when available. (#3845)
-   Log signal messages as debug. (#3851)
-   Log signal messages on media node. (#3852)
-   Log track settings more. (#3853)
-   Update pion deps (#3863)
-   Update golang Docker tag to v1.25 (#3864)
-   Update module github.com/livekit/protocol to v1.40.0 (#3865)
-   Remove unnecessary check (#3870)
-   chunk room updates (#3880)
-   Switch known rids from 012 -> 210, used by OBS. (#3882)
-   init ua parser once (#3883)
-   Revert to using answer for migration case. (#3884)
-   Handle migration better in single peer connection case. (#3886)

## [1.9.0] - 2025-06-02

### Added

-   Add pID and connID to log context to make it easier to search using pID. (#3518)
-   add server agent load threshold config (#3520)
-   Add a key frame seeder in up track. (#3524)
-   Implement SIP update API. (#3141)
-   Add option to use different pacer with send side bwe. (#3552)
-   Allow specifying extra webhooks with egress requests (#3597)

### Fixed

-   Fix missing RTCP sender report when forwarding RED as Opus. (#3480)
-   Take RTT and jitter from receiver view while reporting track stats for (#3483)
-   Fix receiver rtt/jitter. (#3487)
-   fix: fix the wrong error return value (#3493)
-   load mime type before calling writeBlankFrameRTP (#3502)
-   Prevent bind lock deadlock on muted. (#3504)
-   Handle subscribe race with track close better. (#3526)
-   Do not instantiate 0 sized sequencer. (#3529)
-   Fix: Return NotFoundErr instead of Unavailable when the participant does not exist in UpdateParticipant. (#3543)
-   skip out of order participant state updates (#3583)
-   Exclude RED from enabled codecs for Flutter + 2.4.2 + Android. (#3587)
-   protocol update to fix IPv6 SDP fragment parsing (#3603)
-   Forward transfer headers to internal request (#3615)
-   Do not use Redis pipeline for SIP delete. Fixes Redis clustering support. (#3694)

### Changed

-   Use a RED transformer to consolidate both RED -> Opus OR Opus -> RED (#3481)
-   refactor: using slices.Contains to simplify the code (#3495)
-   Do not bind lock across flush which could take time (#3501)
-   Log packet drops/forward. (#3510)
-   Clean up published track on participant removal. (#3527)
-   Do not accept unsupported track type in AddTrack (#3530)
-   Use cgroup for memstats. (#3573)
-   Replace Promise with Fuse. (#3580)
-   Do not drop audio codecs (#3590)
-   map PEER_CONNECTION_DISCONNECTED -> CONNECTION_TIMEOUT (#3591)
-   Update mediatransportutil for max sctp message (65K) (#3611)
-   Disable vp9 for safari 18.4 due to compatibility (#3631)
-   Avoid synthesising duplicate feature. (#3632)
-   Take AudioFeatures from AddTrack. (#3635)
-   Use unordered for lossy data channel. (#3653)
-   Send self participant update immediately. (#3656)
-   update mediatransportutil for sctp congestion control (#3673)

## [1.8.4] - 2025-03-01

### Added

-   Add support for datastream trailer (#3329)
-   Reject ingress if Enabled flag is false (#3293)
-   Use nonce in data messages to de-dupe SendData API. (#3366)
-   H265 supoort and codec regression (#3358)
-   Pass error details and timeouts. (#3402)
-   Webhook analytics event. (#3423)
-   add participant job type (#3443)
-   add datapacket stream metrics (#3450)
-   Implement SIP iterators. (#3332)
-   Add ice candidates logs for failed peerconnection (#3473)

### Fixed

-   Disable SCTP zero checksum for old go client (#3319)
-   disable sctp zero checksum for unknown sdk (#3321)
-   remove code that deletes state from the store for rooms older than 24 hours (#3320)
-   Correct off-by-one lost count on a restart. (#3337)
-   Do not send DD extension if ID is 0. (#3339)
-   allocate node for autocreated room in agent dispatch (#3344)
-   Do not seed if stream is already writable. (#3347)
-   Clone pending tracks to prevent concurrent update. (#3359)
-   Resolve newer participant using higher precision join time. (#3360)
-   Resolve FromAsCasing warning in Dockerfile (#3356)
-   pass RoomConfig along when creating a new dispatch rule (#3367)
-   Reduce chances of metadata cache overflow. (#3369, #3370)
-   ReconnectResponse getting mutated due to mutation of client conf. (#3379)
-   fire TrackSubscribed event only when subscriber is visible (#3378)
-   fix internal signal protocol backward compatibility with 1.7.x (#3384)
-   Correct reason for poor/lost score. (#3397)
-   Do not skip due to large RR interval. (#3398)
-   Update config.go to properly process bool env vars (#3382)
-   consolidated mime type checks (#3407, #3418)
-   Ignore unknown mime in dynacast manager. (#3419)
-   Fix timing issue between track republish (#3428)
-   Catch up if the diff is exactly (1 << 16) also. (#3433)
-   Don't drop message if calculate duration is too small (#3442)
-   Dependent participants should not trigger count towards FirstJoinedAt (#3448)
-   Fix codec regression failed after migration (#3455)
-   Do not revoke track subscription on permission update for exempt participants. (#3458)

### Changed

-   Remove duplicate SSRC get. (#3318)
-   Exempt egress participant from track permissions. (#3322)
-   Use nano time for easier (and hopefully) faster checks/calculations. (#3323)
-   move unrolled mime type check for broader use (#3326)
-   Request key frame if subscribed is higher than max seen and not congested. (#3348)
-   Request key frame on subscription change. (#3349)
-   Room creation time with ms resolution (#3362)
-   close signal session is request messages are undeliverable (#3364)
-   Declare congestion none only if both methods are in DQR. (#3372)
-   Clone TrackInfo to TrackPublishRequested event. (#3377)
-   Run bandwidth estimation when congestion is relieved also (#3380)
-   move ConnectedAt to Participant interface (#3383)
-   Starting on padding for RTX stream is accepted. (#3390)
-   Adjust receiver report sequence number to be within range of highest. (#3396)
-   Split down stream snapshot into sender view and receiver view. (#3422)
-   Seed on receiving forwarder state. (#3435)
-   Give more cache for RTX. (#3438)
-   Properly initialise DD layer selector. (#3467)

## [1.8.3] - 2025-01-07

### Added

-   Allow requesting a dialtone during call transfer (#3122)
-   Handle room configuration that's set in the grant itself (#3120)
-   Update ICE to pick up accepting use-candidate unconditionally for ICE lite agents (#3150)
-   auto create rooms during create agent dispatch api request (#3158)
-   Annotate SIP errors with Twirp codes. (#3161)
-   TWCC based congestion control (#3165 #3234 #3235 #3244 #3245 #3250 #3251 #3253 #3254 #3256 #3262 #3282)
-   Loss based congestion signal detector. (#3168 #3169)
-   Fix header size calculation in stats. (#3171)
-   add per message deflate to signal ws (#3174)
-   Add ResyncDownTracks API that can be used to resync all down tracks on (#3185)
-   One shot signalling mode (#3188 #3192 #3194 #3223)
-   Server side metrics (#3198)
-   Add datastream packet type handling (#3210)
-   Support SIP list filters. (#3240)
-   Add RTX to downstream (#3247)
-   Handle REMB on RTX RTCP (#3257)
-   Thottle the publisher data channel sending when subscriber is slow (#3255 #3265 #3281)

### Fixed

-   avoids NaN (#3119)
-   reduce retransmit by seeding duplicate packets and bytes. (#3124)
-   don't return video/rtx to client (#3142)
-   ignore unexported fields in yaml lint (#3145)
-   Fix incorrect computation of SecondsSinceNodeStatsUpdate (#3172)
-   Attempt to fix missing participant left webhook. (#3173)
-   Set down track connected flag in one-shot-signalling mode. (#3191)
-   Don't SetCodecPreferences for video transceiver (#3249)
-   Disable av1 for safari (#3284)
-   fix completed job status updates causing workers to reconnect (#3294)

### Changed

-   Display both pairs on selected candidate pair change (#3133)
-   Maintain RTT marker for calculations. (#3139)
-   Consolidate operations on LocalNode. (#3140)
-   Use int64 nanoseconds and reduce conversion in a few places (#3159)
-   De-centralize some configs to where they are used. (#3162)
-   Split out audio level config. (#3163)
-   Use int64 nanoseconds and reduce conversion in a few places (#3159)
-   Reduce lock scope. (#3167)
-   Clean up forwardRTP function a bit. (#3177)
-   StreamAllocator (congestion controller) refactor (#3180)
-   convert psprc error to http code in rtc service failure response (#3187)
-   skip http request logging when the client aborts the request (#3195)
-   Do not treat data publisher as publisher. (#3204)
-   Publish data and signal bytes once every 30 seconds. (#3212)
-   upgrade to pion/webrtc v4 (#3213)
-   Don't wait rtp packet to fire track (#3246)
-   Keep negotiated codec parameters in Downtrack.Bind (#3271)
-   Structured logging of ParticipantInit (#3279)
-   Start stream allocator after creating peer connection. (#3283)
-   Reduce memory allocation in WritePaddingRTP / WriteProbePackets (#3288)
-   add room/participant to logger context for SIP APIs (#3290)
-   vp8 temporal layer selection with dependency descriptor (#3302)
-   Use contiguous groups to determine queuing region. (#3308)

## [1.8.0] - 2024-10-18

### Added

-   Support protocol 15 - send signal response for success responses (#2926)
-   Add `DisconnectReason` to `ParticipantInfo`. (#2930)
-   add roommanager service (#2931)
-   Add tracksubscribed event on downtrack added (#2934)
-   Speed up track publication (#2952)
-   Add FastPublish in JoinResponse (#2964)
-   Update protocol. Support SIP Callee dispatch rule type. (#2969)
-   Record out-of-packet count/rate in prom. (#2980)
-   Support passing SIP headers. (#2993)
-   Update ICE via webrtc to get candidate pair stats RTT (#3009)
-   Initial plumbing for metrics. (#2950)
-   Allow agents to override sender identities on ChatMessage (#3022)
-   Implement SIP TransferParticipant API (#3026)
-   api for agent worker job count (#3068)
-   Add counter for pub&sub time metrics (#3084)
-   Support for attributes in initial agent token (#3097)

### Fixed

-   Handle another old packet condition. (#2947)
-   Properly exclude mDNS when not trickling also. (#2956)
-   Panic fix for nil candidate check. (#2957)
-   Skip ICE restart on unestablished peer connection. (#2967)
-   Recreate stats worker on resume if needed. (#2982)
-   Handle trailing slashes in URL (#2988)
-   Do not take padding packets into account in max pps calculation (#2990)
-   compute agent service affinity from available capacity (#2995)
-   Do not remove from subscription map on unsubscribe. (#3002)
-   Fix forwarder panic defer of nil senderReport (#3011)
-   avoid race condition on downtrack.Codec (#3032)
-   fix: copy attributes to refresh token (#3036)
-   Set mime_type for tracks don't have simulcast_codecs (#3040)
-   check data messages for nil payloads (#3062)
-   Fix codec name normalisation. (#3081 #3103 #3104 #3106 #3113)
-   Set FEC enabled properly in connection stats module. (#3098)
-   Type safe IP checks for SIP Trunks. (#3108)
-   Parse python, cpp, unity-web, node sdks in clientinfo (#3110)

### Changed

-   Use monotonic clock in packet path. (#2940)
-   Refactor propagation delay estimator. (#2941)
-   Propagate SIP attributes from a Dispatch Rule. (#2943)
-   Refactor sip create participant (#2949)
-   Reduce threshold of out-of-order very old packet detection. (#2951)
-   Standardize twirp hooks during server init (#2959)
-   Don't remove DD extesion for simucalst codecs (#2960)
-   Negotiate downttrack for subscriber before receiver is ready (#2970)
-   Allow start streaming on an out-of-order packet. (#2971)
-   exponential backoff when calling CreateRoom (#2977)
-   Start negotiate immediately if last one is before debouce interval (#2979)
-   Seed down track state on re-use. (#2985)
-   Cache RTCP sender report in forwarder state. (#2994)
-   Set SenderReport to nil on seeding if empty. (#3008)
-   Use new track id for republishing (#3020)
-   simplify agent registration (#3018)
-   enable room creator service by default (#3043)
-   Fix clock rate skew calculation. (#3055)
-   Forward new disconnect reasons for SIP. (#3056)
-   Use difference debounce interval in negotiation (#3078)
-   Use lower case mime type in dynacast. (#3080)
-   Drop quality a bit faster on score trending lower to be more responsive. (#3093)
-   Protocol update to get more precise protoproxy timing (#3107)

## [1.7.2] - 2024-08-10

### Added

-   Feat add prometheus auth (#2252)
-   Support for Agent protocol v2 (#2786 #2837 #2872 #2886, #2919)
-   Add track subscribed notification to publisher (#2834)
-   Always forward DTMF data messages. (#2848)
-   Send error response when update metadata fails. (#2849)
-   Allow specifying room configuration in token (#2853)
-   Make sender report pass through an option. (#2861)
-   Add option to disable ice lite (#2862)
-   mark final ice candidate (#2871)
-   Send the correct room closed reason to clients (#2901)
-   distribute load to agents probabilistically, inversely proportionate to load (#2902)

### Fixed

-   Fixed participant attributes not broadcasted correctly (#2825)
-   Handle cases of long mute/rollover of time stamp. (#2842)
-   use correct payload type for red primary encoding (#2845)
-   Forward correct payload type for mixed up red/primary payload (#2847)
-   Check size limits on metadata and name set from client. (#2850)
-   Fallback to primary encoding if redundant block overflow (#2858)
-   Support updating local track features when pending. (#2863)
-   don't send unknown signal message to rust sdk with protocol 9 (#2860)
-   Fixed handling of different extensions across multiple media sections (#2878)
-   Fix forced rollover of RTP time stamp. (#2896)
-   Do not start forwarding on an out-of-order packet. (#2917)
-   Reset DD tracker layers when muted. (#2920)

### Changed

-   add handler interface to receive agent worker updates (#2830)
-   log non-trickle candidate in details (#2832)
-   RTP packet validity check. (#2833)
-   Do not warn on padding (#2839)
-   Check sender report against media path. (#2843)
-   Do not create room in UpdateRoomMetadata (#2854)
-   use atomic pointer for MediaTrackReceiver TrackInfo (#2870)
-   Don't create DDParser for non-svc codec (#2883)

## [1.7.0] - 2024-06-23

This version includes a breaking change for SIP service. SIP service now requires `sip.admin` in the token's permission grant
to interact with trunks and dispatch rules; and `sip.call` to dial out to phone numbers.
The latest versions of server SDKs will include the permission grants automatically.

### Added

-   Support new SIP Trunk API. (#2799)
-   Add participant session duration metric (#2801)
-   Support for key/value attributes on Participants (#2806)
-   Breaking: SIP service requires sip.admin or sip.call grants. (#2808)

### Fixed

-   Fixed agent jobs not launching when using the CreateRoom API (#2796)

### Changed

-   Indicate if track is expected to be resumed in `onClose` callback. (#2800)

## [1.6.2] - 2024-06-15

### Added

-   Support for optional publisher datachannel (#2693)
-   add room/participant name limit (#2704)
-   Pass through timestamp in abs capture time (#2715)
-   Support SIP transports. (#2724)

### Fixed

-   add missing strings.EqualFold for some mimeType comparisons (#2701)
-   connection reset without any closing handshake on clientside (#2709)
-   Do not propagate RTCP if report is not processed. (#2739)
-   Fix DD tracker addition. (#2751)
-   Reset tracker on expected layer increase. (#2753)
-   Do not add tracker for invalid layers. (#2759)
-   Do not compare payload type before bind (#2775)
-   fix agent jobs not launching when using the CreateRoom API (#2784)

### Changed

-   Performance improvements to forwarding by using condition var. (#2691 #2699)
-   Simplify time stamp calculation on switches. (#2688)
-   Simplify layer roll back. (#2702)
-   ensure room is running before attempting to delete (#2705)
-   Redact egress object in CreateRoom request (#2710)
-   reduce participant lock scope (#2732)
-   Demote some less useful/noisy logs. (#2743)
-   Stop probe on probe controller reset (#2744)
-   initialize bucket size by publish bitrates (#2763)
-   Validate RTP packets. (#2778)

## [1.6.1] - 2024-04-26

This release changes the default behavior when creating or updating WHIP
ingress. WHIP ingress will now default to disabling transcoding and
forwarding media unchanged to the LiveKit subscribers. This behavior can
be changed by using the new `enable_transcoding` available in updated
SDKs. The behavior of existing ingresses is unchanged.

### Added

-   Add support for "abs-capture-time" extension. (#2640)
-   Add PropagationDelay API to sender report data (#2646)
-   Add support for EnableTranscoding ingress option (#2681)
-   Pass new SIP metadata. Update protocol. (#2683)
-   Handle UpdateLocalAudioTrack and UpdateLocalVideoTrack. (#2684)
-   Forward transcription data packets to the room (#2687)

### Fixed

-   backwards compatability for IsRecorder (#2647)
-   Reduce RED weight in half. (#2648)
-   add disconnected chan to participant (#2650)
-   add typed ops queue (#2655)
-   ICE config cache module. (#2654)
-   use typed ops queue in pctransport (#2656)
-   Use the ingress state updated_at field to ensure that out of order RPC do not overwrite state (#2657)
-   Log ICE candidates to debug TCP connection issues. (#2658)
-   Debug logging addition of ICE candidate (#2659)
-   fix participant, ensure room name matches (#2660)
-   replace keyframe ticker with timer (#2661)
-   fix key frame timer (#2662)
-   Disable dynamic playout delay for screenshare track (#2663)
-   Don't log dd invalid template index (#2664)
-   Do codec munging when munging RTP header. (#2665)
-   Connection quality LOST only if RTCP is also not available. (#2670)
-   Handle large jumps in RTCP sender report timestamp. (#2674)
-   Bump golang.org/x/net from 0.22.0 to 0.23.0 (#2673)
-   do not capture pointers in ops queue closures (#2675)
-   Fix SubParticipant twice when paticipant left (#2672)
-   use ttlcache (#2677)
-   Detach subscriber datachannel to save memory (#2680)
-   Clean up UpdateVideoLayers (#2685)

## [1.6.0] - 2024-04-10

### Added

-   Support for Participant.Kind. (#2505 #2626)
-   Support XR request/response for rtt calculation (#2536)
-   Added support for departureTimeout to keep the room open after participant depart (#2549)
-   Added support for Egress Proxy (#2570)
-   Added support for SIP DTMF data messages. (#2559)
-   Add option to enable bitrate based scoring (#2600)
-   Agent service: support for orchestration v2 & namespaces (#2545 #2641)
-   Ability to disable audio loss proxying. (#2629)

### Fixed

-   Prevent multiple debounce of quality downgrade. (#2499)
-   fix pli throttle locking (#2521)
-   Use the correct snapshot id for PPS. (#2528)
-   Validate SIP trunks and rules when creating new ones. (#2535)
-   Remove subscriber if track closed while adding subscriber. (#2537)
-   fix #2539, do not kill the keepaliveWorker task when the ping timeout occurs (#2555)
-   Improved A/V sync, proper RTCP report past mute. (#2588)
-   Protect duplicate subscription. (#2596)
-   Fix twcc has chance to miss for firefox simulcast rtx (#2601)
-   Limit playout delay change for high jitter (#2635)

### Changed

-   Replace reflect.Equal with generic sliceEqual (#2494)
-   Some optimisations in the forwarding path. (#2035)
-   Reduce heap for dependency descriptor in forwarding path. (#2496)
-   Separate buffer size config for video and audio. (#2498)
-   update pion/ice for tcpmux memory improvement (#2500)
-   Close published track always. (#2508)
-   use dynamic bucket size (#2524)
-   Refactoring channel handling (#2532)
-   Forward publisher sender report instead of generating. (#2572)
-   Notify initial permissions (#2595)
-   Replace sleep with sync.Cond to reduce jitter (#2603)
-   Prevent large spikes in propagation delay (#2615)
-   reduce gc from stream allocator rate monitor (#2638)

## [1.5.3] - 2024-02-17

### Added

-   Added dynamic playout delay if PlayoutDelay enabled in the room (#2403)
-   Allow creating SRT URL pull ingress (requires Ingress service release) (#2416)
-   Use default max playout delay as chrome (#2411)
-   RTX support on publisher transport (#2452)
-   Add exponential backoff to room service check retries (#2462)
-   Add support for ingress ParticipantMetadata (#2461)

### Fixed

-   Prevent race of new track and new receiver. (#2345)
-   Fixed race condition when applying metadata update. (#2363 #2478)
-   Fixed race condition in DownTrack.Bind. (#2388)
-   Improved PSRPC over redis reliability with keepalive (#2398)
-   Fix race condition on Participant.updateState (#2401)
-   Replace /bin/bash with env call for FreeBSD compatibility (#2409)
-   Fix startup with -dev and -config (#2442)
-   Fix published track leaks: close published tracks on participant close (#2446)
-   Enforce empty SID for UserPacket from hidden participants (#2469)
-   Ignore duplicate RID. (Fix for spec breakage by Firefox on Windows 10) (#2471)

### Changed

-   Logging improvements (various PRs)
-   Server shuts down after a second SIGINT to simplify development lifecycle (#2364)
-   A/V sync improvements (#2369 #2437 #2472)
-   Prometheus: larger max session start time bin size (#2380)
-   Updated SIP protocol for creating participants. (requires latest SIP release) (#2404 #2474)
-   Improved reliability of signal stream starts with retries (#2414)
-   Use Deque instead of channels in internal communications to reduce memory usage. (#2418 #2419)
-   Do not synthesise DISCONNECT on session change. (#2412)
-   Prometheus: larger buckets for jitter histogram (#2468)
-   Support for improved Ingress internal RPC (#2485)
-   Let track events go through after participant close. (#2487)

### Removed

-   Removed code related to legacy (pre 1.5.x) RPC protocol (#2384 #2385)

## [1.5.2] - 2023-12-21

Support for LiveKit SIP Bridge

### Added

-   Add SIP Support (#2240 #2241 #2244 #2250 #2263 #2291 #2293)
-   Introduce `LOST` connection quality. (#2265 #2276)
-   Expose detailed connection info with ICEConnectionDetails (#2287)
-   Add Version to TrackInfo. (#2324 #2325)

### Fixed

-   Guard against bad quality in trackInfo (#2271)
-   Group SDES items for one SSRC in the same chunk. (#2280)
-   Avoid dropping data packets on local router (#2270)
-   Fix signal response delivery after session start failure (#2294)
-   Populate disconnect updates with participant identity (#2310)
-   Fix mid info lost when migrating multi-codec simulcast track (#2315)
-   Store identity in participant update cache. (#2320)
-   Fix panic occurs when starting livekit-server with key-file option (#2312) (#2313)

### Changed

-   INFO logging reduction (#2243 #2273 #2275 #2281 #2283 #2285 #2322)
-   Clean up restart a bit. (#2247)
-   Use a worker to report signal/data stats. (#2260)
-   Consolidate TrackInfo. (#2331)

## [1.5.1] - 2023-11-09

Support for the Agent framework.

### Added

-   PSRPC based room and participant service. disabled by default (#2171 #2205)
-   Add configuration to limit MaxBufferedAmount for data channel (#2170)
-   Agent framework worker support (#2203 #2227 #2230 #2231 #2232)

### Fixed

-   Fixed panic in StreamTracker when SVC is used (#2147)
-   fix CreateEgress not completing (#2156)
-   Do not update highest time on padding packet. (#2157)
-   Clear flags in packet metadata cache before setting them. (#2160)
-   Drop not relevant packet only if contiguous. (#2167)
-   Fixed edge cases in SVC codec support (#2176 #2185 #2191 #2196 #2197 #2214 #2215 #2216 #2218 #2219)
-   Do not post to closed channels. (#2179)
-   Only launch room egress once (#2175)
-   Remove un-preferred codecs for android firefox (#2183)
-   Fix pre-extended value on wrap back restart. (#2202)
-   Declare audio inactive if stale. (#2229)

### Changed

-   Defer close of source and sink to prevent error logs. (#2149)
-   Continued AV Sync improvements (#2150 #2153)
-   Egress store/IO cleanup (required for Egress 1.8.0) (#2152)
-   More fine grained filtering NACKs after a key frame. (#2159)
-   Don't filter out ipv6 address for client don't support prflx over relay (#2193)
-   Disable h264 for android firefox (#2190)
-   Do not block on down track close with flush. (#2201)
-   Separate publish and subscribe enabled codecs for finer grained control. (#2217)
-   improve participant hidden (#2220)
-   Reject migration if codec mismatch with published tracks (#2225)

## [1.5.0] - 2023-10-15

### Added

-   Add option to issue full reconnect on data channel error. (#2026)
-   Support non-SVC AV1 track publishing (#2030)
-   Add batch i/o to improve throughput (#2033)
-   Integrate updated TWCC responder (#2038)
-   Allow RoomService.SendData to use participant identities (#2051 #2058)
-   Support for Participant Egress (#2070)
-   Add max playout delay config (#2089)
-   Enable SVC codecs by default (#2109)
-   Add SyncStreams flag to Room, protocol 10 (#2110)

### Fixed

-   Unlock pendingTracksLock when mid is empty (#1994)
-   Do not offer H.264 high profile in subscriber offer, fixes negotiation failures (#1997)
-   Prevent erroneous stream pause. (#2008)
-   Handle duplicate padding packet in the up stream. (#2012)
-   Do not process packets not processed by RTPStats. (#2015)
-   Adjust extended sequence number to account for dropped packets (#2017)
-   Do not force reconnect on resume if there is a pending track (#2081)
-   Fix out-of-range access. (#2082)
-   Start key frame requester on start. (#2111)
-   Handle RED extended sequence number. (#2123)
-   Handle playoutDelay for Firefox (#2135)
-   Fix ICE connection fallback (#2144)

### Changed

-   Drop padding only packets on publisher side. (#1990)
-   Do not generate a stream key for URL pull ingress (#1993)
-   RTPStats optimizations and improvements (#1999 #2000 #2001 #2002 #2003 #2004 #2078)
-   Remove sender report warp logs. (#2007)
-   Don't create new slice when return broadcast downtracks (#2013)
-   Disconnect participant when signal proxy is closed (#2024)
-   Use random NodeID instead of MAC based (#2029)
-   Split RTPStats into receiver and sender. (#2055)
-   Reduce packet meta data cache (#2073 #2078)
-   Reduce ghost participant disconnect timeout (#2077)
-   Per-session TURN credentials (#2080)
-   Use marshal + unmarshal to ensure unmarshallable fields are not copied. (#2092)
-   Allow playout delay even when sync stream isn't used. (#2133)
-   Increase accuracy of delay since last sender report. (#2136)

## [1.4.5] - 2023-08-22

### Added

-   Add ability to roll back video layer selection. (#1871)
-   Allow listing ingress by id (#1874)
-   E2EE trailer for server injected packets. (#1908)
-   Add support for ingress URL pull (#1938 #1939)
-   (experimental) Add control of playout delay (#1838 #1930)
-   Add option to advertise external ip only (#1962)
-   Allow data packet to be sent to participants by identity (#1982)

### Fixed

-   Fix RTC IP when binding to 0.0.0.0 (#1862)
-   Prevent anachronous sample reading in connection stats (#1863)
-   Fixed resubscribe race due to desire changed before cleaning up (#1865)
-   Fixed numPublisher computation by marking dirty after track published changes (#1878)
-   Attempt to avoid out-of-order max subscribed layer notifications. (#1882)
-   Improved packet loss handling for SVC codecs (#1912 )
-   Frame integrity check for SVC codecs (#1914)
-   Issue full reconnect if subscriber PC is closed on ICERestart (#1919)
-   Do not post max layer event for audio. (#1932)
-   Never use dd tracker for non-svc codec (#1952)
-   Fix race condition causing new participants to have stale room metadata (#1969)
-   Fixed VP9 handling for non-SVC content. (#1973)
-   Ensure older session does not clobber newer session. (#1974)
-   Do not start RTPStats on a padding packet. (#1984)

### Changed

-   Push track quality to poor on a bandwidth constrained pause (#1867)
-   AV sync improvements (#1875 #1892 #1944 #1951 #1955 #1956 #1968 #1971 #1986)
-   Do not send unnecessary room updates when content isn't changed (#1881)
-   start reading signal messages before session handler finishes (#1883)
-   changing key file permissions control to allow group readable (#1893)
-   close disconnected participants when signal channel fails (#1895)
-   Improved stream allocator handling during transitions and reallocation. (#1905 #1906)
-   Stream allocator tweaks to reduce re-allocation (#1936)
-   Reduce NACK traffic by delaying retransmission after first send. (#1918)
-   Temper stream allocator more to avoid false negative downgrades (#1920)

## [1.4.4] - 2023-07-08

### Added

-   Add dependency descriptor stream tracker for svc codecs (#1788)
-   Full reconnect on publication mismatch on resume. (#1823)
-   Pacer interface in down stream path. (#1835)
-   retry egress on timeout/resource exhausted (#1852)

### Fixed

-   Send Room metadata updates immediately after update (#1787)
-   Do not send ParticipantJoined webhook if connection was resumed (#1795)
-   Reduce memory leaks by avoiding references in closure. (#1809)
-   Honor bind address passed as `--bind` also for RTC ports (#1815)
-   Avoid dangling downtracks by always deleting them in receiver close. (#1842)
-   Better cleanup of subscriptions with needsCleanup. (#1845)
-   Fix nack issue for svc codecs (#1856)
-   Fixed hidden participant update were still sent when track is published (#1857)
-   Fixed Redis lockup when unlocking room with canceled request context (#1859)

### Changed

-   Improvements to A/V sync (#1773 #1781 #1784 )
-   Improved probing to be less disruptive in low bandwidth scenarios (#1782 #1834 #1839)
-   Do not mute forwarder when paused due to bandwidth congestion. (#1796)
-   Improvements to congestion controller (#1800 #1802 )
-   Close participant on full reconnect. (#1818)
-   Do not process events after participant close. (#1824)
-   Improvements to dependency descriptor based selection forwarder (#1808)
-   Discount out-of-order packets in downstream score. (#1831)
-   Adaptive stream to select highest layer of equal dimensions (#1841)
-   Return 404 with DeleteRoom/RemoveParticipant when deleting non-existent resources (#1860)

## [1.4.3] - 2023-06-03

### Added

-   Send quality stats to prometheus. (#1708)
-   Support for disabling publishing codec on specific devices (#1728)
-   Add support for bypass_transcoding field in ingress (#1741)
-   Include await_start_signal for Web Egress (#1759)

### Fixed

-   Handle time stamp increment across mute for A/V sync (#1705)
-   Additional A/V sync improvements (#1712 #1724 #1737 #1738 #1764)
-   Check egress status on UpdateStream failure (#1716)
-   Start signal relay sessions with the correct node (#1721)
-   Fix unwrap for out-of-order packet (#1729)
-   Fix dynacast for svc codec (#1742 #1743)
-   Ignore receiver reports that have a sequence number before first packet (#1745)
-   Fix node stats updates on Windows (#1748)
-   Avoid reconnect loop for unsupported downtrack (#1754)
-   Perform unsubscribe in parallel to avoid blocking (#1760)

### Changed

-   Make signal close async. (#1711 #1722)
-   Don't add nack if it is already present in track codec (#1714)
-   Tweaked connection quality algorithm to be less sensitive to jitter (#1719)
-   Adjust sender report time stamp for slow publishers (#1740)
-   Split probe controller from StreamAllocator (#1751)

## [1.4.2] - 2023-04-27

### Added

-   VP9 codec with SVC support (#1586)
-   Support for source-specific permissions and client-initiated metadata updates (#1590)
-   Batch support for signal relay (#1593 #1596)
-   Support for simulating subscriber bandwidth (#1609)
-   Support for subscription limits (#1629)
-   Send Room updates when participant counts change (#1647)

### Fixed

-   Fixed process return code to 0 (#1589)
-   Fixed VP9 stutter when not using dependency descriptors (#1595)
-   Fixed stutter when using dependency descriptors (#1600)
-   Fixed Redis cluster support when using Egress or Ingress (#1606)
-   Fixed simulcast parsing error for slower clients (camera and screenshare) (#1621)
-   Don't close RTCP reader if Downtrack will be resumed (#1632)
-   Restore VP8 munger state properly. (#1634)
-   Fixed incorrect node routing when using signal relay (#1645)
-   Do not send hidden participants to others after resume (#1689)
-   Fix for potential webhook delivery delays (#1690)

### Changed

-   Refactored video layer selector (#1588 #1591 #1592)
-   Improved transport fallback when client is resuming (#1597)
-   Improved webhook reliability with delivery retries (#1607 #1615)
-   Congestion controller improvements (#1614 #1616 #1617 #1623 #1628 #1631 #1652)
-   Reduced memory usage by releasing ParticipantInfo after JoinResponse is transmitted (#1619)
-   Ensure safe access in sequencer (#1625)
-   Run quality scorer when there are no streams. (#1633)
-   Participant version is only incremented after updates (#1646)
-   Connection quality attribution improvements (#1653 #1664)
-   Remove disallowed subscriptions on close. (#1668)
-   A/V sync improvements (#1681 #1684 #1687 #1693 #1695 #1696 #1698 #1704)
-   RTCP sender reports every three seconds. (#1692)

### Removed

-   Remove deprecated (non-psrpc) egress client (#1701)

## [1.4.1] - 2023-04-05

### Added

-   Added prometheus metrics for internal signaling API #1571

### Fixed

-   Fix regressions in RTC when using redis with psrpc signaling #1584 #1582 #1580 #1567
-   Fix required bitrate assessment under channel congestion #1577

### Changed

-   Improve DTLS reliability in regions with internet filters #1568
-   Reduce memory usage from logging #1576

## [1.4.0] - 2023-03-27

### Added

-   Added config to disable active RED encoding. Use NACK instead #1476 #1477
-   Added option to skip TCP fallback if TCP RTT is high #1484
-   psrpc based signaling between signal and RTC #1485
-   Connection quality algorithm revamp #1490 #1491 #1493 #1496 #1497 #1500 #1505 #1507 #1509 #1516 #1520 #1521 #1527 #1528 #1536
-   Support for topics in data channel messages #1489
-   Added active filter to ListEgress #1517
-   Handling for React Native and Rust SDK ClientInfo #1544

### Fixed

-   Fixed unsubscribed speakers stuck as speaking to clients #1475
-   Do not include packet in RED if timestamp is too far back #1478
-   Prevent PLI layer lock getting stuck #1481
-   Fix a case of changing video quality not succeeding #1483
-   Resync on pub muted for audio to avoid jump in sequence numbers on unmute #1487
-   Fixed a case of data race #1492
-   Inform reconnecting participant about recently disconnected users #1495
-   Send room update that may be missed by reconnected participant #1499
-   Fixed regression for AV1 forwarding #1538
-   Ensure sequence number continuity #1539
-   Give proper grace period when recorder is still in the room #1547
-   Fix sequence number offset on packet drop #1556
-   Fix signal client message buffer size #1561

### Changed

-   Reduce lock scope getting RTCP sender reports #1473
-   Avoid duplicate queueReconcile in subscription manager #1474
-   Do not log TURN errors with prefix "error when handling datagram" #1494
-   Improvements to TCP fallback mode #1498
-   Unify forwarder between dependency descriptor and no DD case. #1543
-   Increase sequence number cache to handle high rate tracks #1560

## [1.3.5] - 2023-02-25

### Added

-   Allow for strict ACKs to be disabled or subscriber peer connections #1410

### Fixed

-   Don't error when get tc stats fails #1306
-   Fixed support for Redis cluster #1415
-   Fixed unpublished callback being skipped in certain cases #1418
-   Fixed panic when Egress request is sent with an empty output field #1420
-   Do not unsubscribe from track if it's been republished #1424 #1429 #1454 #1465
-   Fixed panic when closing room #1428
-   Use available layers in optimal allocation #1445 #1446 #1448 #1449
-   Fixed unable to notify webhook when egress ending with status EgressStatus_EGRESS_LIMIT_REACHED #1451
-   Reset subscription start timer on permission grant #1457
-   Avoid panic when server receives a token without a video grant #1463

### Changed

-   Updated various logging #1413 #1433 #1437 #1440 #1470
-   Do not force TCP when client left before DTLS handshake #1414
-   Improved performance of data packet forwarding by broadcasting in parallel #1425
-   Cleaning up `availableLayers` and `exemptedLayers` #1407
-   Send stream start on initial start #1456
-   Switch to TLS if ICE/TCP isn't working well #1458

### Removed

-   Removed signal de-duper as it has not proven to be reliable #1427
-   Remove deprecated ingress rpc #1439 (breaking change for Ingress, this will require Ingress v0.0.2+)

## [1.3.4] - 2023-02-09

### Added

-   Memory used and total to node stats #1293 #1296
-   Reconnect response to update ICE servers after resume #1300 #1367
-   Additional prometheus stats #1291
-   Adopt psrpc for internal communication protocol #1295
-   Enable track-level audio nack config #1306
-   Telemetry events for ParticipantResumed, track requested actions #1308
-   Allow disabling mDNS, which degrades performance on certain networks #1311 #1393
-   Publish stream stats to prometheus #1313 #1347
-   Retry initial connection attempt if it fails #1335 #1409
-   Add reconnect reason and signal rtt calculation #1381
-   silent frame for muted audio downtrack #1389

### Fixed

-   Fixed TimedVersion handling of non-monotonic timestamps #1304
-   Persist participant before firing webhook #1340
-   Set IsPublisher to true for data-only publishers #1348
-   Ignore inactive media in SDP #1365
-   Ensure older participant session update does not go out after a newer #1372
-   Fix potentially nil access in buffer #1374
-   Ensure onPacket is not nil in RTCPReader callback #1390
-   Fix rare panic by CreateSenderReport before bind completed #1397

### Changed

-   A/V synchronization improvements #1297 #1315 #1318 #1321 #1351
-   IOInfo service to handle ingress/egress updates #1305
-   Subscription manager to improve subscription resilience #1317 #1358 #1369 #1379 #1382
-   Enable video at low res by default when adaptive stream is enabled #1341
-   Enable upstream nack for opus only audio track #1343
-   Allow /rtc/validate to return room not found message #1344
-   Improve connectivity check to detect DTLS failure #1366
-   Simplify forwarding logic #1349 #1376 #1398
-   Send stream state paused only when it is paused due to bandwidth limitation. #1391
-   Do not catch panics, exit instead to prevent lockup #1392

## [1.3.3] - 2023-01-06

### Added

-   Signal deduper: ignore duplicate signal messages #1243 #1247 #1257
-   FPS based stream tracker #1267 #1269 #1275 #1281
-   Support forwarding track encryption status #1265
-   Use publisher side sender report when forwarding - improves A/V sync #1286

### Fixed

-   When removing a participant, verify SID matches #1237
-   Fixed rare panic when GetSelectedICECandidatePair returns nil #1253
-   Prevent ParticipantUpdate to be sent before JoinResponse #1271 #1272
-   Fixed Firefox connectivity issues when using UDPMux #1270 #1277
-   Fixed subscribing muted track with Egress and Go SDK #1283

### Changed

-   ParticipantLeft webhook would not be sent unless connected successfully #1130
-   Updated to Go 1.18+ #1259
-   Updated Egress RPC framework - psrpc #1252 #1256 #1266 #1273
-   Track subscription operations per source track #1248
-   Egress participants do not count in max_participants #1279

## [1.3.2] - 2022-12-15

### Added

-   help-verbose subcommand to print out all flags #1171 #1180
-   Support for Redis cluster #1181
-   Allow loopback candidates to be used via config option #1185
-   Support for high bitrate audio #1188
-   Ability to detect publication errors and force reconnect #1214
-   API secrets are validated upon startup to ensure sufficient security #1217

### Fixed

-   Correctly suppress verbose pion logs #1163
-   Fixed memory leak on long running room/participants #1169
-   Force full reconnect when there is no previous answer #1168
-   Fixed potential SSRC collision between participants #1173
-   Prevent RTX buffer and forwarding path colliding #1174
-   Do not set forceRelay when unset #1184
-   Prevent subscription after participant close #1182
-   Fixed lost RTCP packets when incorrect buffer factory was used #1195
-   Fixed handling of high bitrate while adding Opus RED #1196
-   Fixes a rare timing issue leading to connection failure #1208
-   Fixed incorrect handling of | in participant identity #1220 #1223
-   Fixed regression causing Firefox to not connect over TURN #1226

### Changed

-   CreateRoom API to allocate the room on RTC node #1155 #1157
-   Check forwarder started when seeding #1191
-   Do not forward media until peer connection is connected #1194
-   Log sampler to reduce log spam #1222

## [1.3.1] - 2022-11-09

### Fixed

-   Fixed logging config causes server to fail to start #1154

## [1.3.0] - 2022-11-08

### Added

-   Ingress Service support #1125
-   Support for web egress #1126
-   Ability to set all configuration params via command line flags #1112
-   Server-side RED encoding for supported clients #1137
-   Opus RED active loss recovery #1139
-   Experimental: fallback to TCP when UDP is unstable #1119
-   Populate memory load in node stats #1121

### Fixed

-   Fixed dynacast pausing a layer due to clients (FF) not publishing layer 0 #1117
-   Room.activeRecording updated correctly after users rejoin #1132
-   Don't collect external candidate IP when it's filtered out #1135
-   Install script to use uname without assuming /usr/bin #1138

### Changed

-   Allocate packetMeta up front to reduce number of allocations #1108
-   Do not log duplicate packet error. #1116
-   Consolidate getMemoryStats #1122
-   Seed snapshots to avoid saving/restoring in downtrack #1128
-   Remove Dependency Descriptor extension when AV1 is not preferred #1129
-   Always send participant updates prior to negotiation #1147
-   Set track level codec settings for all pending tracks #1148
-   Use Redis universal client to support clustered redis #1149

## [1.2.5] - 2022-10-19

### Added

-   Ability to filter IP addresses from being used #1052
-   Allow TCP fallback on multiple connection failures #1077
-   Added support for track level stereo and RED setting #1086

### Fixed

-   Fixed stream allocator with SVC codecs #1053
-   Fixed UDPMux connectivity issues when machine has multiple interfaces #1081
-   Ensure sender reports are in sync after transceiver is re-used #1080
-   Fixed simulcast codec blocking track closure #1082
-   Prevents multiple transport fallback in the same session #1090

### Changed

-   Config validation has been enabled. Server will not start if there are invalid config values #1051
-   Improves NACK stats to count as a miss only if i t's not EOF #1061
-   Store track MIME type during publishing #1065
-   Minor cleanup of media track & friends module #1067
-   Split out shared media transport code into livekit/mediatransportutil #1071
-   Cleaned up logging, improved consistency of debug vs info #1073
-   Reduced memory usage with sequencer #1100
-   Improved IP address mapping, handling of multiple IPs #1094
-   Service API requests are logged #1091
-   Default HTTP handler responds with 404 for unknown paths #1088

## [1.2.3] - 2022-09-13

### Added

-   Supervisor framework to improve edge case & error handling #1005 #1006 #1010 #1017
-   Support for stereo Opus tracks #1013
-   Allow CORS responses to be cached to allow faster initial connection #1027

### Fixed

-   Fixed SSRC mix-up for simulcasted tracks during session resume #1014
-   Fixed screen corruption for non-simulcasted tracks, caused by probing packets #1020
-   Fixed Handling of Simple NALU keyframes for H.264 #1016
-   Fixed TCPMux & UDPMux mixup when multiple host candidates are offered #1036

### Changed

-   Webhook requests are now using Content-Type application/webhook+json to avoid eager JSON parsing #1025
-   Don't automatically add STUN servers when explicit Node IP has been set #1023
-   Automatic TCP and TURN/TLS fallback is now enabled by default #1033

### Removed

-   Fully removed references to VP9. LiveKit is focused on AV1. #1004

## [1.2.1] - 2022-09-13

### Added

-   Accepts existing participant ID on reconnection attempts #988

### Fixed

-   Fixed ICE restart during candidate gathering #963
-   Ensure TrackInfoAvailable is fired after information is known to be ready #967
-   Fixed layer handling when publisher pauses layer 0 (FireFox is has a tendency to pause lowest layer) #984
-   Fixed inaccurate participant count due to storing stale data #992

### Changed

-   Protect against looking up dimensions for invalid spatial layer #977
-   Improvements around migration handling #979 #981 #982 #995
-   Consistent mapping between VideoQuality, rid, and video layers #986
-   Only enable TCP/TURN fallback for supported clients #997

## [1.2.0] - 2022-08-25

### Added

-   Support for NACK with audio tracks (#829)
-   Allow binding HTTP server to specific address, binds to localhost in dev mode(#831)
-   Packet stats from TC (#832)
-   Automatic connectivity fallback to TCP & TURN (#872 #873 #874 #901 #950)
-   Support for client-side ping/pong messages (#871)
-   Support for setCodecPreferences for clients that don't implement it (#916)
-   Opus/RED support: redundant audio transmission is enabled by default (#938 #940)

### Fixed

-   Fixed timing issue in DownTrack.Bind/Close (#833)
-   Fixed TCPMux potentially blocking operations (#840)
-   Fixed ICE restart while still in ICE gathering (#895)
-   Fixed Websocket connection hanging if node isn't available to accept connection (#923)
-   Fixed ICE restart/resume in single node mode (#930)
-   Fixed client disconnected in certain conditions after ICE restart (#932)

### Changed

-   Move to synchronously handle subscriber dynacast status (#834)
-   Retransmit DD extension in case packets were missed (#837)
-   Clean up stats workers (#836)
-   Use TimedVersion for subscription permission updates (#839)
-   Cleaned up logging (#843 #865 #910 #921)
-   track_published event now includes the participant's ID and identity (#846)
-   Improve synchronization of track publishing/unpublish path (#857)
-   Don't re-use transceiver when pending negotiation (#862)
-   Dynacast and media loss proxy refactor (#894 #902)
-   PCTransport refactor (#907 #944)
-   Improve accuracy of connection quality score (#912 #913)
-   Docker image now builds with Go v1.19

## [1.1.2] - 2022-07-11

### Added

-   Returns reason when server disconnects a client (#801 #806)
-   Allow livekit-server to start without keys configuration (#788)
-   Added recovery from negotiation failures (#807)

### Fixed

-   Fixed synchronization issues with Dynacast (#779 #802)
-   Fixed panic due to timing in Pion's ICE agent (#780)
-   ICELite is disabled by default, improving connectivity behind NAT (#784)
-   Fixed EgressService UpdateLayout (#782)
-   Fixed synchronization bugs with selective subscriptions & permissions (#796 #797 #805 #813 #814 #816)
-   Correctly recover from ICE Restart during an negotiation attempt (#798)

### Changed

-   Improved Transceiver re-use to avoid renegotiation (#785)
-   Close room if recorder is the only participant left (#787)
-   Improved connection quality score stability & computation (#793 #795)
-   Set layer state to stopped when paused (#818)

### Removed

-   Removed deprecated RecordingService - Egress should be used instead (#811)

## [1.1.0] - 2022-06-21

### Added

-   Add support for Redis Sentinel (#707)
-   Track participant join total + rate in node stats (#741)
-   Protocol 8 - fast connection support (#747)
-   Simulate switch candidate for network connectivity with poor UDP performance (#754)
-   Allow server to disable codec for certain devices (#755)
-   Support for on-demand multi-codec publishing (#762)

### Fixed

-   Fixed unclean DownTrack close when removed before bound. (#736)
-   Do not munge VP8 header in place - fixes video corruption (#763)

### Changed

-   Reintroduce audio-level quantization to dampen small changes (#732)
-   Allow overshooting maximum when there are no bandwidth constraints. (#739)
-   Improvements to upcoming multi-codec simulcast (#740)
-   Send layer dimensions when max subscribed layers change (#746)
-   Use stable TrackID after unpublishing & republishing (#751)
-   Update egress RPC handler (#759)
-   Improved connection quality metrics (#766 #767 #770 #771 #773 #774 #775)

## [1.0.2] - 2022-05-27

### Changed

-   Fixed edge cases where streams were not allocated (#701)
-   Fixed panic caused by concurrent modifications to stats worker map (#702 #704)
-   Batched subscriber updates to reduce noise in large rooms (#703 #729)
-   Fixed potential data race conditions (#706 #709 #711 #713 #715 #716 #717 #724 #727
-   /debug/pprof endpoint when running in development mode (#708)
-   When audio tracks are muted, send blank frames to induce silence (#710)
-   Fixed stream allocator not upgrading streams after downgrading (#719)
-   Fixed repeated AddSubscriber potentially ignored (#723)
-   Fixed ListEgress API sometimes returning not found (#722)

## [1.0.1] - 2022-05-19

### Changed

-   Update Egress details when changed, fixed Egress APIs (#694)

## [1.0.0] - 2022-05-17

### Added

-   Improved stats around NACKs (#664)
-   Internal structures in preparation for AV1 SVC support (#669)

### Changed

-   Supports participant identity in permissions API (#633)
-   Fixed concurrent access of stats worker map (#666 #670)
-   Do not count padding packets in stream tracker (#667)
-   Fixed TWCC panic under heavy packet loss (#668)
-   Change state to JOINED before sending JoinResponse (#674)
-   Improved frequency of stats update (#673)
-   Send active speaker update during initial subscription (#676)
-   Updated DTLS library to incorporate security fixes (#678)
-   Improved list-nodes command (#681)
-   Improved screen-share handling in StreamTracker (#683)
-   Inject slience opus packets when muted (#682)
