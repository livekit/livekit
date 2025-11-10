
2.15.0 / 2024-11-26
===================
  * Expose `grammar` object to allow injecting custom rules #103

2.14.2 / 2024-01-29
===================
  * Release a new version to prove we actually exist #102

2.14.1 / 2020-12-02
===================
  * Fix `a=rtcp-fb` bug where trr-int is `*` via #91

2.14.0 / 2020-01-22
===================
  * Add `a=ptime` now support float values for sub-ms values via #89

2.13.0 / 2019-09-29
===================
  * Add `a=ts-refclk` and `a=mediaclk` for RFC7273 support via #78

2.12.0 / 2019-08-05
===================
  * a=extmap-allow-mixed (RFC 8285) #87

2.11.0 / 2019-07-28
===================
  * BFCP RFC4583 support via #86

2.10.0 / 2019-07-12
===================
  * `a=connection` support for RFC4145 via #85

2.9.0 / 2019-07-11
==================
  * `a=keywds` support via #82

2.8.0 / 2019-05-29
==================
  * `a=extmap` `encrypt-uri` improvements in #81
  * `parsePayloads` safe parsing bugfix for integer equivalent m-lines #80

2.7.0 / 2018-11-21
==================
  * `a=sctp-port` + `a=max-message-size` support added in #76 via @egzonzeneli

2.6.0 / 2018-11-14
==================
  * `a=label` support added in #75 via @jeremy-j-ackso

2.5.0 / 2018-11-02
==================
  * `a=bundle-only` support added in #73 via @ibc

2.4.1 / 2018-04-02
==================
  * `parseParams` now doesn't break on name only params #70 via @manuc66

2.4.0 / 2018-01-24
==================
  * `a=source-filter` support added in #69 via @thosil

2.3.1 / 2018-01-05
==================
  * `a=ssrc` bug attributes including dashes fixed in #68 via @MichelSimonot

2.3.0 / 2017-03-06
==================
  * `a=framerate` from rfc4566 now parseable - #63 via @gregpabian

2.2.0 / 2017-03-05
==================
  * `a=rid` now parseable - #59 from @ibc
  * `parseFmtpConfig` now aliased as `parseParams` - works on a more general level - #60
  * `parseFmtpConfig` deprecated - will be removed in 3.0.0
  * `a=imageattr` now parseable - #61 from @ibc
  * `parseImageattrParams` for extended image attr parsing RFC6236 - #61
  * `a=simulcast` now parseable (both draft version 3 and draft v7) - #62 from @ibc
  - `parseSimulcastStreamList` for more detailed simulcast parsing - #62

2.1.0 / 2017-03-02
==================
  * `a=x-google-flag:%s` now parseable - #58 via @ibc

2.0.1 / 2017-02-20
==================
  * a=ssrc-group parsing now doesn't break on dash-separation #54 via @murillo128

2.0.0 / 2017-02-16
==================
  * a=extmap lines now parsed into a 4 object struct rather than a broken 3 object compound struct - #51 via @ibc
  * this is unlikely to be breaking, but we major bumped just to be sure

1.7.0 / 2016-12-09
==================
  * a=ssrc lines now properly handle attributes without values - #40 via @zxcpoiu
  * a=candidate now supports network-id and network-cost values - #49 via @zxcpoiu

1.6.2 / 2016-03-23
==================
  * Fix `a=rtpmap` parsing when codec included dots - #44 via @alexanderklintstrom

1.6.1 / 2016-03-18
==================
  * Fix parsing of fmtp parameters with base64 in `parseFmtpConfig` - #42 via @lmoj

1.6.0 / 2016-03-02
==================
  * Add support for `a=sctpmap` - #41 via @6uliver

1.5.3 / 2015-11-25
==================
 * Parse tcp ice-candidates with raddr + rport correctly - #37 via @damencho

1.5.2 / 2015-11-17
==================
  * Parse tcp ice-candidates lines correctly - #35 via @virtuacoplenny

1.5.1 / 2015-11-15
==================
  * Added `.npmignore`

1.5.0 / 2015-09-05
==================
  * Suport AirTunes a=rtpmap lines without clockrate #30 - via @DuBistKomisch

1.4.1 / 2015-08-14
==================
  * Proper handling of whitespaces in a=fmtp: lines #29 - via @bgrozev
  * `parseFmtpConfig` helper also handles whitespaces properly

1.4.0 / 2015-03-18
==================
  * Add support for `a=rtcp-rsize`

1.3.0 / 2015-03-16
==================
  * Add support for `a=end-of-candidates` trickle ice attribute

1.2.1 / 2015-03-15
==================
  * Add parsing for a=ssrc-group

1.2.0 / 2015-03-05
==================
  * a=msid attributes support and msid-semantic improvements
  * writer now ignores `undefined` or `null` values

1.1.0 / 2014-10-20
==================
  * Add support for parsing session level `a=ice-lite`

1.0.0 / 2014-09-30
==================
  * Be more lenient with nelines. Allow \r\n, \r or \n.

0.6.1 / 2014-07-25
==================
  * Documentation and test coverage release

0.6.0 / 2014-02-18
==================
  * invalid a= lines are now parsed verbatim in `media[i].invalid` (#19)
  * everything in `media[i].invalid` is written out verbatim (#19)
  * add basic RTSP support (a=control lines) (#20)

0.5.3 / 2014-01-17
==================
  * ICE candidates now parsed fully (no longer ignoring optional attrs) (#13)

0.5.2 / 2014-01-17
==================
  * Remove `util` dependency to help browserify users
  * Better parsing of `a=extmap`, `a=crypto` and `a=rtcp-fb` lines
  * `sdp-verify` bin file included to help discover effects of `write ∘ parse`

0.5.1 / 2014-01-16
==================
  * Correctly parse a=rtpmap with telephone-event codec #16
  * Correctly parse a=rtcp lines that conditionally include the IP #16

0.5.0 / 2014-01-14
==================
  * Enforce spec mandated \r\n line endings over \n (#15)
  * Parsing of opus rtpmap wrong because encoding parameters were discarded (#12)

0.4.1 / 2013-12-19
==================
  * Changed 'sendrecv' key on media streams to be called 'direction' to match SDP related RFCs (thanks to @saghul)

0.3.3 / 2013-12-10
==================
  * Fixed a bug that caused time description lines ("t=" and "z=") to be in the wrong place

0.3.2 / 2013-10-21
==================
  * Fixed a bug where large sessionId values where being rounded (#8)
  * Optionally specify the `outerOrder` and `innerOrder` for the writer (allows working around Chrome not following the RFC specified order in #7)

0.3.1 / 2013-10-19
==================
  * Fixed a bug that meant the writer didn't write the last newline (#6)

0.3.0 / 2013-10-18
==================
  * Changed ext grammar to parse id and direction as one (fixes writing bug)
  * Allow mid to be a string (fixes bug)
  * Add support for maxptime value
  * Add support for ice-options
  * Add support for grouping frameworks
  * Add support for msid-semantic
  * Add support for ssrc
  * Add support for rtcp-mux
  * Writer improvements: add support for session level push attributes

0.2.1 / 2013-07-31
==================
  * Support release thanks to @legastero, following was pulled from his fork:
  * Add support for rtcp-fb attributes.
  * Add support for header extension (extmap) attributes.
  * Add support for crypto attributes.
  * Add remote-candidates attribute support and parser.

0.2.0 / 2013-07-27
==================
  * parse most normal lines sensibly
  * factored out grammar properly
  * added a writer that uses common grammar
  * stop preprocessing parse object explicitly (so that parser ∘ writer == Id)
    these parser helpers are instead exposed (may in the future be extended)

0.1.0 / 2013-07-21
==================
  * rewrite parsing mechanism
  * parse origin lines more efficiently
  * parsing output now significantly different

0.0.2 / 2012-07-18
==================
  * ice properties parsed

0.0.1 / 2012-07-17
==================
  * Original release
