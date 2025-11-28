#!/usr/bin/env node

var transform = require('./')
  , file = require('path').join(process.cwd(), process.argv[2])
  , sdp = require('fs').readFileSync(file).toString()
  , parsed = transform.parse(sdp)
  , written = transform.write(parsed)
  , writtenLines = written.split('\r\n')
  , origLines = sdp.split('\r\n')
  , numMissing = 0
  , numNew = 0
  ;

var parseFails = 0;
parsed.media.forEach(function (media) {
  (media.invalid || []).forEach(function (inv) {
    console.warn('unrecognized a=' + inv.value + ' belonging to m=' + media.type);
    parseFails += 1;
  });
});
var parseStr = parseFails + ' unrecognized line(s) copied blindly';

origLines.forEach(function (line, i) {
  if (writtenLines.indexOf(line) < 0) {
    console.error('l' + i + ' lost (' + line + ')');
    numMissing += 1;
  }
});

writtenLines.forEach(function (line, i) {
  if (origLines.indexOf(line) < 0) {
    console.error('l' + i + ' new (' + line + ')');
    numNew += 1;
  }
});

var failed = (numMissing > 0 || numNew > 0);
if (failed) {
  console.log('\n' + file + ' changes during transform:');
  console.log(numMissing + ' missing line(s), ' + numNew + ' new line(s)%s',
    parseFails > 0 ? ', ' + parseStr : ''
  );
}
else {
  console.log(file + ' verified%s', parseFails > 0 ? ', but had ' + parseStr : '');
}
process.exit(failed ? 1 : 0);
