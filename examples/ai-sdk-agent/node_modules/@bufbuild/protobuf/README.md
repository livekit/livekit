# @bufbuild/protobuf

This package provides the runtime library for the code generator plugin
[protoc-gen-es](https://www.npmjs.com/package/@bufbuild/protoc-gen-es).

## Protocol Buffers for ECMAScript

A complete implementation of [Protocol Buffers](https://developers.google.com/protocol-buffers) in TypeScript,
suitable for web browsers and Node.js.

**Protobuf-ES** is intended to be a solid, modern alternative to existing Protobuf implementations for the JavaScript ecosystem.  It is the first project in this space to provide a comprehensive plugin framework and decouple the base types from RPC functionality.

Some additional features that set it apart from the others:

- ECMAScript module support
- First-class TypeScript support
- Generation of idiomatic JavaScript and TypeScript code.
- Generation of [much smaller bundles](https://github.com/bufbuild/protobuf-es/blob/main/packages/bundle-size)
- Implementation of all proto3 features, including the [canonical JSON format](https://developers.google.com/protocol-buffers/docs/proto3#json).
- Implementation of all proto2 features, except for extensions and the text format.  
- Usage of standard JavaScript APIs instead of the [Closure Library](http://googlecode.blogspot.com/2009/11/introducing-closure-tools.html)
- Compatibility is covered by the protocol buffers [conformance tests](https://github.com/bufbuild/protobuf-es/blob/main/packages/protobuf-conformance).
- Descriptor and reflection support

## Installation

```bash
npm install @bufbuild/protobuf
```

## Documentation

To learn how to work with `@bufbuild/protobuf` check out the docs for the [Runtime API](https://github.com/bufbuild/protobuf-es/blob/main/docs/runtime_api.md)
and the [generated code](https://github.com/bufbuild/protobuf-es/blob/main/docs/generated_code.md).

Official documentation for the Protobuf-ES project can be found at [github.com/bufbuild/protobuf-es](https://github.com/bufbuild/protobuf-es).

For more information on Buf, check out the official [Buf documentation](https://docs.buf.build/introduction).

## Examples

A complete code example can be found in the **Protobuf-ES** repo [here](https://github.com/bufbuild/protobuf-es/tree/main/packages/protobuf-example).

