// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

const fs = require('node:fs');
const path = require('node:path');

const sourcePath = process.argv[2];
if (!sourcePath) throw new Error('pass unicode_skeleton 0.1.1/src/data.rs');
const source = fs.readFileSync(sourcePath, 'utf8');
const indicesMatch = source.match(/INPUT_AND_OUTPUT_INDICES:[^=]+ = \[(.*?)\];/s);
const outputsMatch = source.match(/OUTPUTS:[^=]+ = \[(.*?)\];/s);
if (!indicesMatch || !outputsMatch) throw new Error('unsupported unicode-skeleton data format');

const indices = [...indicesMatch[1].matchAll(/\((\d+),\s*(\d+)\)/g)].map(match => [Number(match[1]), Number(match[2])]);
const tokens = [...outputsMatch[1].matchAll(/'(?:\\.|[^'\\])*'/g)].map(match => match[0]);
const outputs = tokens.map(token => {
  const value = token.slice(1, -1);
  if (value.startsWith('\\u{') && value.endsWith('}')) return String.fromCodePoint(parseInt(value.slice(3, -1), 16));
  const escapes = { "\\'": "'", '\\"': '"', '\\\\': '\\', '\\n': '\n', '\\r': '\r', '\\t': '\t', '\\0': '\0' };
  return escapes[value] ?? value;
});
if (indices.length !== 6294 || outputs.length !== 8990) throw new Error(`unexpected table dimensions: ${indices.length}/${outputs.length}`);

const lines = [
  '// Code generated from unicode_skeleton 0.1.1 data (Unicode Security Mechanisms 10.0.0).',
  '// Copyright 2016 Peter Reid; licensed under MIT. See third_party/unicode-skeleton/LICENSE-MIT.',
  '// DO NOT EDIT.',
  'package invitelinks',
  '',
  'var unicodeSkeletonMappings = map[rune]string{',
];
for (let index = 0; index < indices.length; index++) {
  const [codepoint, offset] = indices[index];
  const end = indices[index + 1]?.[1] ?? outputs.length;
  lines.push(`\t0x${codepoint.toString(16)}: ${JSON.stringify(outputs.slice(offset, end).join(''))},`);
}
lines.push('}');
const outputPath = path.resolve(__dirname, '../internal/invitelinks/unicode_skeleton_data.go');
fs.writeFileSync(outputPath, lines.join('\n') + '\n');
