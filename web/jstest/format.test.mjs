// Self-check for the pure format.js helpers. Run: node format.test.mjs
// No DOM harness in this area, so only DOM-free helpers are covered.
import assert from "node:assert/strict";
import { csvField } from "../static/app/format.js";

// Formula-injection neutralization (CWE-1236): a leading trigger char gets a ' prefix.
for (const trig of ["=", "+", "-", "@", "\t", "\r"]) {
    assert.equal(csvField(trig + "1+1"), "'" + trig + "1+1", `trigger ${JSON.stringify(trig)}`);
}
// Real attack payload: no comma/dquote/newline, so only the ' prefix is added.
assert.equal(csvField("=2+5+cmd|'/c calc'!A1"), "'=2+5+cmd|'/c calc'!A1");

// Normal values pass through untouched.
assert.equal(csvField("example.com"), "example.com");
assert.equal(csvField("A record"), "A record");

// RFC 4180 quoting still applies (comma / quote / newline).
assert.equal(csvField("a,b"), '"a,b"');
assert.equal(csvField('he said "hi"'), '"he said ""hi"""');
assert.equal(csvField("line1\nline2"), '"line1\nline2"');

// Nullish and arrays.
assert.equal(csvField(null), "");
assert.equal(csvField(undefined), "");
assert.equal(csvField(["a", "b"]), "a;b");
// Array whose joined form starts with a trigger is still neutralized.
assert.equal(csvField(["=danger", "x"]), "'=danger;x");

console.log("format.test.mjs: all assertions passed");
