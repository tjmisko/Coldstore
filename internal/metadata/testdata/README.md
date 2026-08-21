# Metadata test fixtures

`iphone.heic` is a genuine iPhone 15 Pro HEIC copied byte-for-byte from
`github.com/bep/imagemeta@v1.0.0/testdata/images/iphone.heic` (SHA-256
`0cdba68ea035d8be5e2042718754fa35610e7846001c13085bf4b3734c11ae47`).
It is copyright 2026 Toni Melisma and licensed under
[CC BY-SA 4.0](https://creativecommons.org/licenses/by-sa/4.0/). The fixture is
kept intact so it continues to exercise a real Apple maker note, HEIC boxes,
capture offset, subsecond, GPS altitude, GPS accuracy, and ContentIdentifier.

`quicktime.mov` is a one-frame 16x16 black video generated solely for this test
suite with FFmpeg's `color` source and `libopenh264`. It contains synthetic
QuickTime creation, ISO 6709 location, and ContentIdentifier tags and is
dedicated to the public domain under
[CC0 1.0](https://creativecommons.org/publicdomain/zero/1.0/).

`no-exif.jpg` is copied byte-for-byte from
`github.com/evanoberholster/imagemeta@v1.0.0/assets/NoExif.jpg` (SHA-256
`2597cd8faff9d7bcf2959751ee07d7740be194aa23b4e948134b8e83d9ecc7b2`).
It is redistributed under that project's MIT license, reproduced in
`evan-imagemeta-LICENSE.txt`.

