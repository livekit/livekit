# TypeScript implementation of debounce function

![Build Status](https://github.com/chodorowicz/ts-debounce//workflows/node-ci/badge.svg)
[![npm](https://img.shields.io/npm/v/ts-debounce.svg)](https://www.npmjs.com/package/ts-debounce)
[![npm bundle size (minified + gzip)](https://img.shields.io/bundlephobia/minzip/ts-debounce.svg)](https://www.npmjs.com/package/ts-debounce)
[![npm type definitions](https://img.shields.io/npm/types/ts-debounce.svg)](https://www.npmjs.com/package/ts-debounce)

<!-- ALL-CONTRIBUTORS-BADGE:START - Do not remove or modify this section -->

[![All Contributors](https://img.shields.io/badge/all_contributors-6-orange.svg?style=flat-square)](#contributors-)

<!-- ALL-CONTRIBUTORS-BADGE:END -->

_Debounce_ creates a new function `g`, which when called will delay the invocation of the original function `f` until `n` milliseconds, BUT drop previous pending delayed emissions if a new invocation is made before `n` milliseconds.

It's very useful for scenarios when it's better to limit the number of times the function is called. E.g. think of search input which fetches data from API. It's enough display search results after user has stopped entering characters for some time.

## Install

You can install this package using `npm` using the command below

```bash
npm install ts-debounce
```

If you prefer `yarn`, install using the command below

```bash
yarn add ts-debounce
```

## Function arguments

```ts
import { debounce } from "ts-debounce";

const debouncedFunction = debounce(originalFunction, waitMilliseconds, options);
```

- `originalFunction`
  - the function which we want to debounce
- `waitMilliseconds`
  - how many seconds must pass after most recent function call, for the original function to be called
- `options`
  - `isImmediate` (boolean)
    - if set to `true` then `originalFunction` will be called immediately, but on subsequent calls of the debounced function original function won't be called, unless `waitMilliseconds` passed after last call
  - `maxWait` (number)
    - if defined it will call the `originalFunction` after `maxWait` time has passed, even if the debounced function is called in the meantime
      - e.g. if `wait` is set to `100` and `maxWait` to `200`, then if the debounced function is called every 50ms, then the original function will be called after 200ms anyway
  - `callback` (function)
    - it is called when `originalFunction` is debounced and receives as first parameter returned data from `originalFunction`

## Cancellation

The returned debounced function can be cancelled by calling `cancel()` on it.

```ts
const debouncedFunction = debounce(originalFunction, waitMilliseconds, options);

debouncedFunction.cancel();
```

## Promises

Since v3 `ts-debounce` has Promise support. Everytime you call debounced function a promise is returned which will be resolved when the original function will be finally called. This promise will be rejected, if the debounced function will be cancelled.

You can also debounce a function `f` which returns a promise. The returned promise(s) will resolve (unless cancelled) with the return value of the original function.

```ts
const asyncFunction = async () => "value";
const g = debounce(asyncFunction);
const returnValue = await g();
returnValue === "value"; // true
```

## Credits & inspiration

This implementation is based upon following sources:

- [JavaScript Debounce Function](https://davidwalsh.name/javascript-debounce-function) by David Walsh
- [Lodash implementation](https://lodash.com/)
- [p-debounce](https://github.com/sindresorhus/p-debounce)

## Compability

- version 3 - Promise must be available in the global namespace
- version 2 - TypeScript 3.3
- version 1 - TypeScript 2.0

## Contributors ✨

Thanks goes to these wonderful people ([emoji key](https://allcontributors.org/docs/en/emoji-key)):

<!-- ALL-CONTRIBUTORS-LIST:START - Do not remove or modify this section -->
<!-- prettier-ignore-start -->
<!-- markdownlint-disable -->
<table>
  <tr>
    <td align="center"><a href="http://zacharysvoboda.com"><img src="https://avatars3.githubusercontent.com/u/5839548?v=4?s=100" width="100px;" alt=""/><br /><sub><b>Zac</b></sub></a><br /><a href="https://github.com/chodorowicz/ts-debounce/commits?author=zacnomore" title="Code">💻</a></td>
    <td align="center"><a href="https://github.com/karol-majewski"><img src="https://avatars1.githubusercontent.com/u/20233319?v=4?s=100" width="100px;" alt=""/><br /><sub><b>Karol Majewski</b></sub></a><br /><a href="https://github.com/chodorowicz/ts-debounce/commits?author=karol-majewski" title="Code">💻</a></td>
    <td align="center"><a href="https://github.com/Tuizi"><img src="https://avatars2.githubusercontent.com/u/2027148?v=4?s=100" width="100px;" alt=""/><br /><sub><b>Fabien Rogeret</b></sub></a><br /><a href="https://github.com/chodorowicz/ts-debounce/commits?author=Tuizi" title="Code">💻</a></td>
    <td align="center"><a href="https://github.com/iheidari"><img src="https://avatars3.githubusercontent.com/u/1315090?v=4?s=100" width="100px;" alt=""/><br /><sub><b>Iman</b></sub></a><br /><a href="https://github.com/chodorowicz/ts-debounce/commits?author=iheidari" title="Code">💻</a></td>
    <td align="center"><a href="https://github.com/juanmendes"><img src="https://avatars.githubusercontent.com/u/549331?v=4?s=100" width="100px;" alt=""/><br /><sub><b>juanmendes</b></sub></a><br /><a href="#ideas-juanmendes" title="Ideas, Planning, & Feedback">🤔</a></td>
    <td align="center"><a href="https://github.com/sanduluca"><img src="https://avatars.githubusercontent.com/u/56228534?v=4?s=100" width="100px;" alt=""/><br /><sub><b>sanduluca</b></sub></a><br /><a href="https://github.com/chodorowicz/ts-debounce/commits?author=sanduluca" title="Code">💻</a></td>
  </tr>
</table>

<!-- markdownlint-restore -->
<!-- prettier-ignore-end -->

<!-- ALL-CONTRIBUTORS-LIST:END -->

This project follows the [all-contributors](https://github.com/all-contributors/all-contributors) specification. Contributions of any kind welcome!
