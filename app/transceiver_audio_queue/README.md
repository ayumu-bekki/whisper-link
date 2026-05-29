# transceiver_audio_queue

## ビルド手順

### 必要なシステムライブラリ

標準の `apt` リポジトリに含まれていないパッケージがあるため、Debian の公式パッケージサーバーから直接インストールする。

```bash
# libopus-dev (Opus コーデックの開発ファイル)
wget http://ftp.debian.org/debian/pool/main/o/opus/libopus-dev_1.5.2-2_arm64.deb -O /tmp/libopus-dev.deb
sudo dpkg -i /tmp/libopus-dev.deb

# libprotoc (protoc の共有ライブラリ)
wget http://ftp.debian.org/debian/pool/main/p/protobuf/libprotoc32t64_3.21.12-11_arm64.deb -O /tmp/libprotoc32t64.deb
sudo dpkg -i /tmp/libprotoc32t64.deb

# libprotobuf-dev (Protocol Buffers の開発ファイル)
wget http://ftp.debian.org/debian/pool/main/p/protobuf/libprotobuf-dev_3.21.12-11_arm64.deb -O /tmp/libprotobuf-dev.deb
sudo dpkg -i /tmp/libprotobuf-dev.deb

# protobuf-compiler (protoc コマンド本体)
wget http://ftp.debian.org/debian/pool/main/p/protobuf/protobuf-compiler_3.21.12-11_arm64.deb -O /tmp/protobuf-compiler.deb
sudo dpkg -i /tmp/protobuf-compiler.deb
```

### ビルド

```bash
cargo build
```

### 動作確認環境

- Raspberry Pi (aarch64)
- Debian trixie
