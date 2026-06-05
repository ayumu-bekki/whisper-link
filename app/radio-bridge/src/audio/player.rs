use alsa::pcm::{Access, Format, HwParams, PCM};
use alsa::{Direction, ValueOr};
use ogg::reading::PacketReader;
use opus::Decoder;
use std::io::Cursor;
use tracing::debug;

const ALSA_SAMPLE_RATE: u32 = 48000; // ALSAデバイスは48kHz固定
const OPUS_SAMPLE_RATE: u32 = 24000; // Opusデコードは24kHz
const MAX_DECODE_SIZE: usize = 2880; // 最大フレームサイズ: 120ms @ 24kHz

pub struct AudioPlayer {
    pub device: String,
}

impl AudioPlayer {
    pub fn new(device: &str) -> Self {
        Self {
            device: device.to_string(),
        }
    }

    /// Ogg Opusデータをデコードしてブロッキング再生する。
    /// 再生完了まで呼び出し元をブロックする。
    pub fn play_blocking(&self, ogg_opus_data: &[u8]) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
        let pcm_data = decode_ogg_opus(ogg_opus_data)?;
        debug!("decoded {} samples", pcm_data.len());
        self.write_to_alsa(&pcm_data)?;
        Ok(())
    }

    fn write_to_alsa(&self, samples: &[i16]) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
        // 24kHz → 48kHz アップサンプル (各サンプルを2回繰り返す)
        let upsampled: Vec<i16> = samples.iter().flat_map(|&s| [s, s]).collect();

        let pcm = PCM::new(&self.device, Direction::Playback, false)?;

        {
            let hwp = HwParams::any(&pcm)?;
            hwp.set_channels(1)?;
            hwp.set_rate(ALSA_SAMPLE_RATE, ValueOr::Nearest)?;
            hwp.set_format(Format::s16())?;
            hwp.set_access(Access::RWInterleaved)?;
            pcm.hw_params(&hwp)?;
        }

        let io = pcm.io_i16()?;

        let mut offset = 0;
        while offset < upsampled.len() {
            let chunk = &upsampled[offset..];
            match io.writei(chunk) {
                Ok(written) => offset += written,
                Err(e) => {
                    pcm.try_recover(e, false)?;
                }
            }
        }

        pcm.drain()?;
        Ok(())
    }
}

fn decode_ogg_opus(data: &[u8]) -> Result<Vec<i16>, Box<dyn std::error::Error + Send + Sync>> {
    let mut cursor = Cursor::new(data);
    let mut reader = PacketReader::new(&mut cursor);
    let mut decoder = Decoder::new(OPUS_SAMPLE_RATE, opus::Channels::Mono)?;
    let mut output = Vec::new();
    let mut header_packets = 0usize;

    loop {
        match reader.read_packet()? {
            None => break,
            Some(packet) => {
                // 最初の2パケットはOpusヘッダ (OpusHead, OpusTags)
                if header_packets < 2 {
                    header_packets += 1;
                    continue;
                }

                let mut buf = vec![0i16; MAX_DECODE_SIZE];
                let decoded = decoder.decode(&packet.data, &mut buf, false)?;
                output.extend_from_slice(&buf[..decoded]);
            }
        }
    }

    Ok(output)
}
