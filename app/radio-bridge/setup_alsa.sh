#!/bin/sh
# USB オーディオデバイスの ALSA キャプチャゲインを設定する。
# radio-bridge を起動する前にホスト上で一度実行すること。
# ゲイン設定は ALSA の永続化機構 (alsactl store) で保存されるため、
# 再起動後も有効だが、デバイス差し替え時は再実行が必要。
#
# 使い方:
#   sudo sh setup_alsa.sh

set -e

CARD=2
NUMID=4
VALUE=50  # 30/60 = 15dB (背景ノイズが閾値以下に収まる推奨値)

echo "Setting Mic Capture Volume (card=$CARD numid=$NUMID) to $VALUE/60..."
amixer -c "$CARD" cset numid="$NUMID" "$VALUE","$VALUE"

echo "Saving ALSA state..."
alsactl store "$CARD"

echo "Done."
