FROM ubuntu:24.04@sha256:561618e2c15bf2397621dd04f96926663a3b5616c189cf7e38db7e82f5c538ea AS runtime-packages
# Reviewed Ubuntu package bytes and the Chrome for Testing headless shell
# archive for each target; see cli/RUNTIME-PACKAGES.md.
ADD --checksum=sha256:7e05959b067031468f21ae46c6653a6813f1dccd994ef12a0b5f75de0ed346b6 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/a/at-spi2-core/at-spi2-common_2.52.0-1build1_all.deb /runtime-packages/all/at-spi2-common.deb
ADD --checksum=sha256:6bac2a01979e210d9eac1d4d56747ec709ea60654744d66705dc3c36e7629e50 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/c/ca-certificates/ca-certificates_20260601~24.04.1_all.deb /runtime-packages/all/ca-certificates.deb
ADD --checksum=sha256:9df0579cd298a1f2b7a40efd519c849745aecd193a09733cf4d1032284a62bbf https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/f/fontconfig/fontconfig-config_2.15.0-1.1ubuntu2_amd64.deb /runtime-packages/amd64/fontconfig-config.deb
ADD --checksum=sha256:86ea76c38d2168989fbcf22792ca1e786643a971ffb87248ca9dfcf0c3c7d1e1 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/f/fontconfig/fontconfig-config_2.15.0-1.1ubuntu2_arm64.deb /runtime-packages/arm64/fontconfig-config.deb
ADD --checksum=sha256:40049660c194f3b8a2541fc7369efebb10e9f94bdac836a2f38fafedd10fa73a https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/f/fonts-dejavu/fonts-dejavu-core_2.37-8_all.deb /runtime-packages/all/fonts-dejavu-core.deb
ADD --checksum=sha256:8a599d6553307db7ecb795d2f0e5a301e03234afc75c7358b0ba43466454c89a https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/f/fonts-dejavu/fonts-dejavu-mono_2.37-8_all.deb /runtime-packages/all/fonts-dejavu-mono.deb
ADD --checksum=sha256:0c26896193480d9dbb055e85e8ddfb9c9f34cf19afee340987cd6311e2886fc1 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/a/alsa-lib/libasound2-data_1.2.11-1ubuntu0.3_all.deb /runtime-packages/all/libasound2-data.deb
ADD --checksum=sha256:5d17bfb5683eb99f1be951fe48e1be5bf3c52391913b6b74a8aecf9b4f5e779f https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/a/alsa-lib/libasound2t64_1.2.11-1ubuntu0.3_amd64.deb /runtime-packages/amd64/libasound2t64.deb
ADD --checksum=sha256:de49425b8480c651eae0ffbe24a1f1d88b2ea578f95a291b95f78fae10662ec6 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/a/alsa-lib/libasound2t64_1.2.11-1ubuntu0.3_arm64.deb /runtime-packages/arm64/libasound2t64.deb
ADD --checksum=sha256:22b7d47e3c0f7953a78d3cfd309d1b20771085de9220fbe610c0989b9e808c9b https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/a/at-spi2-core/libatk-bridge2.0-0t64_2.52.0-1build1_amd64.deb /runtime-packages/amd64/libatk-bridge2.0-0t64.deb
ADD --checksum=sha256:7a2ce532caa857c4d7d8bced0a8143d1c7cf8a3093f8da926e025252ef70117f https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/a/at-spi2-core/libatk-bridge2.0-0t64_2.52.0-1build1_arm64.deb /runtime-packages/arm64/libatk-bridge2.0-0t64.deb
ADD --checksum=sha256:42c5d4b00954f17c2c3c4b866844f691eb8b6d57bf08b59203e65f12dc84a4f9 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/a/at-spi2-core/libatk1.0-0t64_2.52.0-1build1_amd64.deb /runtime-packages/amd64/libatk1.0-0t64.deb
ADD --checksum=sha256:8939633b3912c6476f3f501c2e8aa0fcc681dbd343b0a8138bf0ed5af8e9a77a https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/a/at-spi2-core/libatk1.0-0t64_2.52.0-1build1_arm64.deb /runtime-packages/arm64/libatk1.0-0t64.deb
ADD --checksum=sha256:d686ace4080eca9c2d6ce8de69392a5ac56c846701894c0858922d4ce341a1d7 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/a/at-spi2-core/libatspi2.0-0t64_2.52.0-1build1_amd64.deb /runtime-packages/amd64/libatspi2.0-0t64.deb
ADD --checksum=sha256:c65f137507a665c969f9d158c8f63188b308037e66b48bca85ea2e7cf31bb84e https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/a/at-spi2-core/libatspi2.0-0t64_2.52.0-1build1_arm64.deb /runtime-packages/arm64/libatspi2.0-0t64.deb
ADD --checksum=sha256:f3857b0863ac5cfd4263e9bf6cfb1d4be88d5321e4070d5bc2b62b0949e6c86f https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libb/libbsd/libbsd0_0.12.1-1build1.1_amd64.deb /runtime-packages/amd64/libbsd0.deb
ADD --checksum=sha256:cdf9ff601244e64482584d9d9b98aa54a573c081ab5956227c737307196c1ce3 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libb/libbsd/libbsd0_0.12.1-1build1.1_arm64.deb /runtime-packages/arm64/libbsd0.deb
ADD --checksum=sha256:5d630480f04b4b442300ce847a3fa705ea4d14d80ba6de91f99b51a4e4953b08 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/d/dbus/libdbus-1-3_1.14.10-4ubuntu4.1_amd64.deb /runtime-packages/amd64/libdbus-1-3.deb
ADD --checksum=sha256:c907237716178bea0afb61845d7bcb3c716803830df4537e0bc6633b56903a7c https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/d/dbus/libdbus-1-3_1.14.10-4ubuntu4.1_arm64.deb /runtime-packages/arm64/libdbus-1-3.deb
ADD --checksum=sha256:0b60996bc18fa1545e60a47bb0fe1a7f360ec06fe62ba35fb8d8ba2a54e62166 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libd/libdrm/libdrm-common_2.4.125-1ubuntu0.1~24.04.2_all.deb /runtime-packages/all/libdrm-common.deb
ADD --checksum=sha256:4d3e858dd57be617c49d87c5ddcdb35df744a4f0a559981256ae2ba174718584 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libd/libdrm/libdrm2_2.4.125-1ubuntu0.1~24.04.2_amd64.deb /runtime-packages/amd64/libdrm2.deb
ADD --checksum=sha256:6ed8a3f9da5fdc36a289503341a608c21fb33b77acf1604c83265c3f70ca9018 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libd/libdrm/libdrm2_2.4.125-1ubuntu0.1~24.04.2_arm64.deb /runtime-packages/arm64/libdrm2.deb
ADD --checksum=sha256:494b8e672f722130c6bca6a7bc4cc31a43ca891a31d60d868bfdd699a3c20b13 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/e/expat/libexpat1_2.6.1-2ubuntu0.6_amd64.deb /runtime-packages/amd64/libexpat1.deb
ADD --checksum=sha256:9782206e359ccd50dd5686c2fd5182ed87c777a6d76b0269e38e89dbaac9f3b2 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/e/expat/libexpat1_2.6.1-2ubuntu0.6_arm64.deb /runtime-packages/arm64/libexpat1.deb
ADD --checksum=sha256:bffad21d2383429d6dbec8395ba8d8476a0ed69e7e0b1c3ef2f27da99211b200 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/m/mesa/libgbm1_24.0.5-1ubuntu1_amd64.deb /runtime-packages/amd64/libgbm1.deb
ADD --checksum=sha256:8589f317e7dacff6b958b9a2395fda3c9d62304b3da1390a53adbdb3afb33436 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/m/mesa/libgbm1_24.0.5-1ubuntu1_arm64.deb /runtime-packages/arm64/libgbm1.deb
ADD --checksum=sha256:a66ef54888a18e20b55dd3988177f6029caa81dc16198c043a715500b62cf06f https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/g/glib2.0/libglib2.0-0t64_2.80.0-6ubuntu3.9_amd64.deb /runtime-packages/amd64/libglib2.0-0t64.deb
ADD --checksum=sha256:e8ed0414ac46a406c7b8c18b28572075dcc6807b1cfc48fa88198c0db0a7dcf9 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/g/glib2.0/libglib2.0-0t64_2.80.0-6ubuntu3.9_arm64.deb /runtime-packages/arm64/libglib2.0-0t64.deb
ADD --checksum=sha256:e579e72d091f6c7a13f5a756c31065b15aae5b81840d61b069355aa2283c07b4 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/n/nspr/libnspr4_4.35-1.1build1_amd64.deb /runtime-packages/amd64/libnspr4.deb
ADD --checksum=sha256:0bd5994126c41aa05aa380954a3cb2ae56a0a8ebe368a0e01a411fdcc0cb9c68 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/n/nspr/libnspr4_4.35-1.1build1_arm64.deb /runtime-packages/arm64/libnspr4.deb
ADD --checksum=sha256:4254f11d782dfb970e78113519a59d440112ed43c4b56f709da0aef81da8651a https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/n/nss/libnss3_3.98-1ubuntu0.2_amd64.deb /runtime-packages/amd64/libnss3.deb
ADD --checksum=sha256:daf32de5e12139aa84fde7a3e2784d5d7277259e22f14affcd82d81545b5312e https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/n/nss/libnss3_3.98-1ubuntu0.2_arm64.deb /runtime-packages/arm64/libnss3.deb
ADD --checksum=sha256:b1190bb72359f5fcc47406aa46065eaf4f1ca208085c51224a52b04bedc0b4bb https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/s/sqlite3/libsqlite3-0_3.45.1-1ubuntu2.8_amd64.deb /runtime-packages/amd64/libsqlite3-0.deb
ADD --checksum=sha256:85925089ef6820a5b6ed80bb490d187465e49687c388333895680fda54e3ee06 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/s/sqlite3/libsqlite3-0_3.45.1-1ubuntu2.8_arm64.deb /runtime-packages/arm64/libsqlite3-0.deb
ADD --checksum=sha256:3d0955bc049bbcca0f4c3e78a3a8b994593d96db7d84f4320217224433844534 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/o/openssl/libssl3t64_3.0.13-0ubuntu3.15_amd64.deb /runtime-packages/amd64/libssl3t64.deb
ADD --checksum=sha256:bf0b51fb18187c4e1b03900e1049dc219047080ff511aa3bf691dbe24e14d24a https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/o/openssl/libssl3t64_3.0.13-0ubuntu3.15_arm64.deb /runtime-packages/arm64/libssl3t64.deb
ADD --checksum=sha256:edbfa4b6857691ae922cc768a753fab230eee9e956aa2ce4f2eaa8d9ad777dca https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/w/wayland/libwayland-server0_1.22.0-2.1build1_amd64.deb /runtime-packages/amd64/libwayland-server0.deb
ADD --checksum=sha256:2b6dec0d40070685f4ce346d20b064fc5e118a3149f30be88465251985b496ed https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/w/wayland/libwayland-server0_1.22.0-2.1build1_arm64.deb /runtime-packages/arm64/libwayland-server0.deb
ADD --checksum=sha256:397f84347476a3c5786b39f3ff6f0f82866eb3d8be6d2ad3efeadf019efe5b80 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libx11/libx11-6_1.8.7-1build1_amd64.deb /runtime-packages/amd64/libx11-6.deb
ADD --checksum=sha256:16d10ae5d27f1a0feb054001ff2b213b51acd29894f86ff088c31eebeba65fe3 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libx11/libx11-6_1.8.7-1build1_arm64.deb /runtime-packages/arm64/libx11-6.deb
ADD --checksum=sha256:9ae01f7747e7f479394c697c55acf11d3baf139e8e54b7c4adfb55ef9c50de08 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libx11/libx11-data_1.8.7-1build1_all.deb /runtime-packages/all/libx11-data.deb
ADD --checksum=sha256:e40d29f1d1a62393bacaedebe0da3d9006084152a9f7e5e029293f08ce1c5c80 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxau/libxau6_1.0.9-1build6_amd64.deb /runtime-packages/amd64/libxau6.deb
ADD --checksum=sha256:348d0732ff9571f95d6ff18373a93e827bfe00474d792ef5e9071e6dd618605c https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxau/libxau6_1.0.9-1build6_arm64.deb /runtime-packages/arm64/libxau6.deb
ADD --checksum=sha256:b5cb519823a05a617b543dc9b6a9289b7648b20048244abe021caa706309835a https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxcb/libxcb-randr0_1.15-1ubuntu2_amd64.deb /runtime-packages/amd64/libxcb-randr0.deb
ADD --checksum=sha256:d1dc1d06b42da9a332274e34121ff57c6f6c1c155e4fe726c652f80f392ed419 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxcb/libxcb-randr0_1.15-1ubuntu2_arm64.deb /runtime-packages/arm64/libxcb-randr0.deb
ADD --checksum=sha256:e1c6611d11ad7398326f1bf028afc34c3b14c51d917a3426b966ed4b9687fa58 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxcb/libxcb1_1.15-1ubuntu2_amd64.deb /runtime-packages/amd64/libxcb1.deb
ADD --checksum=sha256:5483e5d19bbfa28e961d951645944e7763e943262ac17e216b2870d83597f319 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxcb/libxcb1_1.15-1ubuntu2_arm64.deb /runtime-packages/arm64/libxcb1.deb
ADD --checksum=sha256:a12d5d9ae70b798cbbf284080eae32b87367561fc9c24492f641b8860ac0f308 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxcomposite/libxcomposite1_0.4.5-1build3_amd64.deb /runtime-packages/amd64/libxcomposite1.deb
ADD --checksum=sha256:94f0d6d602d00e9ac53d307509897672f1132c875e2576a06fd9f3dcd00dd81e https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxcomposite/libxcomposite1_0.4.5-1build3_arm64.deb /runtime-packages/arm64/libxcomposite1.deb
ADD --checksum=sha256:6891faf325e996ebd28ef53ebb9c043dc96f67327500490affc3e89d366d35ed https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxdamage/libxdamage1_1.1.6-1build1_amd64.deb /runtime-packages/amd64/libxdamage1.deb
ADD --checksum=sha256:6f54865fd13492e6bd9045e18c48270cd6c422c447c5531a17358a26748e5d50 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxdamage/libxdamage1_1.1.6-1build1_arm64.deb /runtime-packages/arm64/libxdamage1.deb
ADD --checksum=sha256:bcd336fce11ce2a45f34d0f95e6980af22529f22147e8f98c156e5cee8ee42bb https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxdmcp/libxdmcp6_1.1.3-0ubuntu6_amd64.deb /runtime-packages/amd64/libxdmcp6.deb
ADD --checksum=sha256:6b96dbbae4e515ac8f32d6dacfcb6afd1266118400a85ae7cb3f887895017f53 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxdmcp/libxdmcp6_1.1.3-0ubuntu6_arm64.deb /runtime-packages/arm64/libxdmcp6.deb
ADD --checksum=sha256:45783969a9ece9d7b7b733b8c60981584c53c6bc5ee3b42d295d2f80d1285679 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxext/libxext6_1.3.4-1build2_amd64.deb /runtime-packages/amd64/libxext6.deb
ADD --checksum=sha256:1140abacd8d23cb83cc63fb9aebb15a201e2dddbf5ca79fd999c0cbd181d0088 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxext/libxext6_1.3.4-1build2_arm64.deb /runtime-packages/arm64/libxext6.deb
ADD --checksum=sha256:0ee1015cccd063249e01c0cd0bf45f513c8ac9a1e5e485070c12e69192455e4a https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxfixes/libxfixes3_6.0.0-2build1_amd64.deb /runtime-packages/amd64/libxfixes3.deb
ADD --checksum=sha256:884fc6560c5a8bc91fb6b7d067f557d8088ccee86ad1974b6a57085ac121e8de https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxfixes/libxfixes3_6.0.0-2build1_arm64.deb /runtime-packages/arm64/libxfixes3.deb
ADD --checksum=sha256:0ea7acb5e8a8ce4d6653b30e03a319bfe136db7bfb5ee7cad34c2aa1272ea8d9 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxi/libxi6_1.8.1-1build1_amd64.deb /runtime-packages/amd64/libxi6.deb
ADD --checksum=sha256:aeeb74a6ff6ea16312fd8eb1988b1ddf32afbb4503ab7121763530ee700058e3 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxi/libxi6_1.8.1-1build1_arm64.deb /runtime-packages/arm64/libxi6.deb
ADD --checksum=sha256:2b9caeb423efb540296a1cb20b872cc630c23908407ecb5c1c787a617622d664 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxkbcommon/libxkbcommon0_1.6.0-1build1_amd64.deb /runtime-packages/amd64/libxkbcommon0.deb
ADD --checksum=sha256:4ef68b9d56971a3119f40684f52a200be5c7c1bf8c5ef430a2d502e910bf8b61 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxkbcommon/libxkbcommon0_1.6.0-1build1_arm64.deb /runtime-packages/arm64/libxkbcommon0.deb
ADD --checksum=sha256:f2955a5e594f5724b58ad241d9231ea191cb36574a0d5e5ca6b661cd41d6256d https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxrandr/libxrandr2_1.5.2-2build1_amd64.deb /runtime-packages/amd64/libxrandr2.deb
ADD --checksum=sha256:7470632764f1fde987e760850c2be58973e265f41b2bb8015c415d247898d2c4 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxrandr/libxrandr2_1.5.2-2build1_arm64.deb /runtime-packages/arm64/libxrandr2.deb
ADD --checksum=sha256:d70bd831aebe8d4834b5dd2ed98df26dd6bd27f1042c47543bd7f66df1ae22ea https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxrender/libxrender1_0.9.10-1.1build1_amd64.deb /runtime-packages/amd64/libxrender1.deb
ADD --checksum=sha256:0a3aedab15d9b0d7a74b0b043676f0caacc104f188be16a008196dbc20e9e2e2 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/libx/libxrender/libxrender1_0.9.10-1.1build1_arm64.deb /runtime-packages/arm64/libxrender1.deb
ADD --checksum=sha256:1f2401db59a0beffde6cc47bea3664965ab8fa206b7a5ac8f1cc649cfeeb056b https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/o/openssl/openssl_3.0.13-0ubuntu3.15_amd64.deb /runtime-packages/amd64/openssl.deb
ADD --checksum=sha256:818b4ab7092eb3365358ce41896ad08206b30cdb67445d7ec0503262893a0d95 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/o/openssl/openssl_3.0.13-0ubuntu3.15_arm64.deb /runtime-packages/arm64/openssl.deb
ADD --checksum=sha256:a505b9d491386167bd8e14e3383315a4a7d6539e4406745901ccf009a7988271 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/u/unzip/unzip_6.0-28ubuntu4.1_amd64.deb /runtime-packages/amd64/unzip.deb
ADD --checksum=sha256:6c03087d20f67ec7333b65090e4bbcb7f680d4c045b1e81c8421634e87297d23 https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/u/unzip/unzip_6.0-28ubuntu4.1_arm64.deb /runtime-packages/arm64/unzip.deb
ADD --checksum=sha256:e99f2a0b4c56cdf35dec489e56b6a8418826cab8ce6306ec6d0bd98e2cd0014a https://snapshot.ubuntu.com/ubuntu/20260927T000000Z/pool/main/x/xkeyboard-config/xkb-data_2.41-2ubuntu1.1_all.deb /runtime-packages/all/xkb-data.deb
ADD --checksum=sha256:f90329720fb61e5a76da9b6b509b6ad12a484c9c5df0359403e935177c6d153a https://storage.googleapis.com/chrome-for-testing-public/157.0.8103.78/linux64/chrome-headless-shell-linux64.zip /runtime-packages/amd64/chrome-headless-shell.zip
ADD --checksum=sha256:912fbf877e53de34d7b3cca5104f3b95607d41bd35412ccc6cd2a599ff0c0dbf https://storage.googleapis.com/chrome-for-testing-public/157.0.8103.78/linux-arm64/chrome-headless-shell-linux-arm64.zip /runtime-packages/arm64/chrome-headless-shell.zip

FROM ubuntu:24.04@sha256:561618e2c15bf2397621dd04f96926663a3b5616c189cf7e38db7e82f5c538ea
# https://www.docker.com/blog/faster-multi-platform-builds-dockerfile-cross-compilation-guide/

ARG TARGETPLATFORM
ARG warp_env
ENV WARP_ENV=$warp_env
# The gift card renderer runs this headless shell, the same Chrome for Testing
# version on both targets.
ENV WARP_GIFT_CHROMIUM=/opt/chrome-headless-shell/chrome-headless-shell

# Install offline and unpack the target's headless shell offline, add the
# unprivileged user the gift card printer runs Chromium as (giftcard, uid and gid
# 10001, no home, no shell, no password; written as fixed lines so the image
# stays reproducible: useradd would stamp the day into /etc/shadow), then
# discard build-only inode cache and wall-clock log.
# Keep the runtime linker cache, package database and generated certificates.
RUN --network=none --mount=from=runtime-packages,source=/runtime-packages,target=/runtime-packages,ro \
    dpkg -i /runtime-packages/"$(dpkg --print-architecture)"/*.deb /runtime-packages/all/*.deb \
    && update-ca-certificates \
    && if [ "$(dpkg --print-architecture)" = amd64 ]; then unzip -q /runtime-packages/amd64/chrome-headless-shell.zip -d /opt && mv /opt/chrome-headless-shell-linux64 /opt/chrome-headless-shell; fi \
    && if [ "$(dpkg --print-architecture)" = arm64 ]; then unzip -q /runtime-packages/arm64/chrome-headless-shell.zip -d /opt && mv /opt/chrome-headless-shell-linux-arm64 /opt/chrome-headless-shell; fi \
    && echo 'giftcard:x:10001:10001:gift card printer:/nonexistent:/usr/sbin/nologin' >> /etc/passwd \
    && echo 'giftcard:x:10001:' >> /etc/group \
    && echo 'giftcard:!:::::::' >> /etc/shadow \
    && echo 'giftcard:!::' >> /etc/gshadow \
    && rm -f /var/cache/ldconfig/aux-cache /var/log/dpkg.log

COPY build/$TARGETPLATFORM/taskworker /usr/local/sbin/bringyour-taskworker

RUN ln -s /srv/warp/vault/.aws /root/.aws

STOPSIGNAL SIGTERM
CMD ["/usr/local/sbin/bringyour-taskworker", "-p", "80"]
