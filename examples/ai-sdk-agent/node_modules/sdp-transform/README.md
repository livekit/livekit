# SDP Transform

[![npm status](http://img.shields.io/npm/v/sdp-transform.svg)](https://www.npmjs.org/package/sdp-transform)
[![CI](https://github.com/clux/sdp-transform/actions/workflows/ci.yml/badge.svg)](https://github.com/clux/sdp-transform/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/clux/sdp-transform/graph/badge.svg?token=OqDbVhIP3f)](https://codecov.io/gh/clux/sdp-transform)

A simple parser and writer of SDP. Defines internal grammar based on [RFC4566 - SDP](http://tools.ietf.org/html/rfc4566), [RFC5245 - ICE](http://tools.ietf.org/html/rfc5245), and many more.

For simplicity it will force values that are integers to integers and leave everything else as strings when parsing. The module should be simple to extend or build upon, and is constructed rigorously.


## Installation

```bash
$ npm install sdp-transform
```


## TypeScript Definitions

Available in the [@types/sdp-transform](https://www.npmjs.com/package/@types/sdp-transform) package:

```bash
$ npm install -D @types/sdp-transform
```


## Usage

Load using CommonJS syntax or ES6 syntax:

```js
// CommonJS
const sdpTransform = require('sdp-transform');

// ES6
import * as sdpTransform from 'sdp-transform';
```


## Usage - Parser

Pass it an unprocessed SDP string.

```js
const sdpStr = "v=0\r\n\
o=- 20518 0 IN IP4 203.0.113.1\r\n\
s= \r\n\
t=0 0\r\n\
c=IN IP4 203.0.113.1\r\n\
a=ice-ufrag:F7gI\r\n\
a=ice-pwd:x9cml/YzichV2+XlhiMu8g\r\n\
a=fingerprint:sha-1 42:89:c5:c6:55:9d:6e:c8:e8:83:55:2a:39:f9:b6:eb:e9:a3:a9:e7\r\n\
m=audio 54400 RTP/SAVPF 0 96\r\n\
a=rtpmap:0 PCMU/8000\r\n\
a=rtpmap:96 opus/48000\r\n\
a=ptime:20\r\n\
a=sendrecv\r\n\
a=candidate:0 1 UDP 2113667327 203.0.113.1 54400 typ host\r\n\
a=candidate:1 2 UDP 2113667326 203.0.113.1 54401 typ host\r\n\
m=video 55400 RTP/SAVPF 97 98\r\n\
a=rtpmap:97 H264/90000\r\n\
a=fmtp:97 profile-level-id=4d0028;packetization-mode=1\r\n\
a=rtpmap:98 VP8/90000\r\n\
a=sendrecv\r\n\
a=candidate:0 1 UDP 2113667327 203.0.113.1 55400 typ host\r\n\
a=candidate:1 2 UDP 2113667326 203.0.113.1 55401 typ host\r\n\
";

const res = sdpTransform.parse(sdpStr);
// =>
{ version: 0,
  origin:
   { username: '-',
     sessionId: 20518,
     sessionVersion: 0,
     netType: 'IN',
     ipVer: 4,
     address: '203.0.113.1' },
  name: '',
  timing: { start: 0, stop: 0 },
  connection: { version: 4, ip: '203.0.113.1' },
  iceUfrag: 'F7gI',
  icePwd: 'x9cml/YzichV2+XlhiMu8g',
  fingerprint:
   { type: 'sha-1',
     hash: '42:89:c5:c6:55:9d:6e:c8:e8:83:55:2a:39:f9:b6:eb:e9:a3:a9:e7' },
  media:
   [ { rtp: [Object],
       fmtp: [],
       type: 'audio',
       port: 54400,
       protocol: 'RTP/SAVPF',
       payloads: '0 96',
       ptime: 20,
       direction: 'sendrecv',
       candidates: [Object] },
     { rtp: [Object],
       fmtp: [Object],
       type: 'video',
       port: 55400,
       protocol: 'RTP/SAVPF',
       payloads: '97 98',
       direction: 'sendrecv',
       candidates: [Object] } ] }


// each media line is parsed into the following format
res.media[1];
// =>
{ rtp:
   [ { payload: 97,
       codec: 'H264',
       rate: 90000 },
     { payload: 98,
       codec: 'VP8',
       rate: 90000 } ],
  fmtp:
   [ { payload: 97,
       config: 'profile-level-id=4d0028;packetization-mode=1' } ],
  type: 'video',
  port: 55400,
  protocol: 'RTP/SAVPF',
  payloads: '97 98',
  direction: 'sendrecv',
  candidates:
   [ { foundation: 0,
       component: 1,
       transport: 'UDP',
       priority: 2113667327,
       ip: '203.0.113.1',
       port: 55400,
       type: 'host' },
     { foundation: 1,
       component: 2,
       transport: 'UDP',
       priority: 2113667326,
       ip: '203.0.113.1',
       port: 55401,
       type: 'host' } ] }
```

In this example, only slightly dodgy string coercion case here is for `candidates[i].foundation`, which can be a string, but in this case can be equally parsed as an integer.

### Parser Postprocessing

No excess parsing is done to the raw strings apart from maybe coercing to ints, because the writer is built to be the inverse of the parser. That said, a few helpers have been built in:

#### parseParams()

Parses `fmtp.config` and others such as `rid.params` and returns an object with all the params in a key/value fashion.

```js
// to parse the fmtp.config from the previous example
sdpTransform.parseParams(res.media[1].fmtp[0].config);
// =>
{ 'profile-level-id': '4d0028',
  'packetization-mode': 1 }
```

#### parsePayloads()

Returns an array with all the payload advertised in the main m-line.

```js
// what payloads where actually advertised in the main m-line ?
sdpTransform.parsePayloads(res.media[1].payloads);
// =>
[97, 98]
```

#### parseImageAttributes()

Parses [Generic Image Attributes](https://tools.ietf.org/html/rfc6236). Must be provided with the `attrs1` or `attrs2` string of a `a=imageattr` line. Returns an array of key/value objects.

```js
// a=imageattr:97 send [x=1280,y=720] recv [x=1280,y=720] [x=320,y=180]
sdpTransform.parseImageAttributes(res.media[1].imageattrs[0].attrs2)
// =>
[ {'x': 1280, 'y': 720}, {'x': 320, 'y': 180} ]
```

#### parseSimulcastStreamList()

Parses [simulcast](https://tools.ietf.org/html/draft-ietf-mmusic-sdp-simulcast) streams/formats. Must be provided with the `list1` or `list2` string of the `a=simulcast` line.

Returns an array of simulcast streams. Each entry is an array of alternative simulcast formats, which are objects with two keys:

* `scid`: Simulcast identifier
* `paused`: Whether the simulcast format is paused

```js
// a=simulcast:send 1,~4;2;3 recv c
sdpTransform.parseSimulcastStreamList(res.media[1].simulcast.list1);
// =>
[
  // First simulcast stream (two alternative formats)
  [ {scid: 1, paused: false}, {scid: 4, paused: true} ],
  // Second simulcast stream
  [ {scid: 2, paused: false} ],
  // Third simulcast stream
  [ {scid: 3, paused: false} ]
]
```

## Usage - Writer

The writer is the inverse of the parser, and will need a struct equivalent to the one returned by it.

```js
sdpTransform.write(res).split('\r\n'); // res parsed above
// =>
[ 'v=0',
  'o=- 20518 0 IN IP4 203.0.113.1',
  's= ',
  'c=IN IP4 203.0.113.1',
  't=0 0',
  'a=ice-ufrag:F7gI',
  'a=ice-pwd:x9cml/YzichV2+XlhiMu8g',
  'a=fingerprint:sha-1 42:89:c5:c6:55:9d:6e:c8:e8:83:55:2a:39:f9:b6:eb:e9:a3:a9:e7',
  'm=audio 54400 RTP/SAVPF 0 96',
  'a=rtpmap:0 PCMU/8000',
  'a=rtpmap:96 opus/48000',
  'a=ptime:20',
  'a=sendrecv',
  'a=candidate:0 1 UDP 2113667327 203.0.113.1 54400 typ host',
  'a=candidate:1 2 UDP 2113667326 203.0.113.1 54401 typ host',
  'm=video 55400 RTP/SAVPF 97 98',
  'a=rtpmap:97 H264/90000',
  'a=rtpmap:98 VP8/90000',
  'a=fmtp:97 profile-level-id=4d0028;packetization-mode=1',
  'a=sendrecv',
  'a=candidate:0 1 UDP 2113667327 203.0.113.1 55400 typ host',
  'a=candidate:1 2 UDP 2113667326 203.0.113.1 55401 typ host' ]
```

The only thing different from the original input is we follow the order specified by the SDP RFC, and we will always do so.

## Usage - Custom grammar

In case you need to add custom grammar (e.g. add unofficial attributes) to the parser, you can do so by mutating the `grammar` object before parsing.

```js
sdpTransform.grammar['a'].push({
  name: 'xCustomTag',
  reg: /^x-custom-tag:(\d*)/,
  names: ['tagId'],
  format: 'x-custom-tag:%d
})
```

## License

MIT-Licensed. See LICENSE file for details.
