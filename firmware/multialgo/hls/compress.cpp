// SPDX-License-Identifier: MIT
// One algorithm per image; the 64-bit shell interface is identical in all images.
#include <ap_axi_sdata.h>
#include <hls_stream.h>
#include <stdint.h>

#ifndef CF_ALGORITHM
#error CF_ALGORITHM must be 3 (LZ4) or 4 (Zstd)
#endif

typedef ap_axiu<64, 0, 0, 0> Input;
typedef ap_axiu<64, 32, 0, 0> Output;

#if CF_ALGORITHM == 4
#include "zstd_compress_multicore.hpp"

// AXI types belong at top-level ports. Retain one beat of vector data so TLAST
// accompanies the last real bytes, including an exact 64-bit final word.
static void finish_zstd(hls::stream<IntVectorStream_dt<8,8> >& input,
                        hls::stream<Output>& output) {
    ap_uint<128> buffer=0;
    unsigned count=0;
    for (auto value=input.read(); value.strobe; value=input.read()) {
#pragma HLS PIPELINE II=1
        for (unsigned i=0;i<8;++i) {
#pragma HLS UNROLL
            buffer.range((count+i)*8+7,(count+i)*8)=value.data[i];
        }
        count+=value.strobe;
        if (count>8) {
            Output out;out.data=buffer.range(63,0);out.keep=255;out.strb=255;out.user=0;out.last=false;
            output.write(out);buffer>>=64;count-=8;
        }
    }
    Output out;out.data=buffer.range(63,0);out.keep=(1u<<count)-1;out.strb=out.keep;out.user=0;out.last=true;
    output.write(out);
}

void compress_ctl(hls::stream<Input>& inStream, hls::stream<Output>& outStream) {
#pragma HLS INTERFACE axis port=inStream
#pragma HLS INTERFACE axis port=outStream
#pragma HLS INTERFACE ap_ctrl_hs port=return
#pragma HLS DATAFLOW
    hls::stream<IntVectorStream_dt<8,8> > raw("raw"), encoded("encoded");
#pragma HLS STREAM variable=raw depth=32
#pragma HLS STREAM variable=encoded depth=32
    // Each enabled DMA engine distributes independent blocks to four LZ77
    // lanes and shares an entropy encoder. The shell exposes two DMA engines.
    xf::compression::details::zstdAxiu2hlsStream<64>(inStream, raw);
    xf::compression::zstdCompressQuadCore<4, 8192, 8192, 128>(raw, encoded);
    finish_zstd(encoded, outStream);
}

#elif CF_ALGORITHM == 3
#include "lz_compress.hpp"
#include "lz_optional.hpp"
#include "lz4_compress.hpp"

static const unsigned BLOCK = 32768;

static void load_bytes(const uint8_t raw[BLOCK], unsigned size, hls::stream<ap_uint<8> >& output) {
    for (unsigned i = 0; i < size; ++i) {
#pragma HLS PIPELINE II=1
        output.write(raw[i]);
    }
}

static void collect(hls::stream<ap_uint<8> >& input, hls::stream<bool>& eos,
                    hls::stream<uint32_t>& sizes, uint8_t encoded[BLOCK], unsigned& size) {
    size = 0;
    bool end = eos.read();
    while (!end) {
#pragma HLS PIPELINE II=1
        encoded[size++] = input.read();
        end = eos.read();
    }
    (void)input.read(); // upstream's dummy EOS byte
    (void)sizes.read();
}

static void compress_block(const uint8_t raw[BLOCK], unsigned size, uint8_t encoded[BLOCK], unsigned& encoded_size) {
#pragma HLS DATAFLOW
    hls::stream<ap_uint<8> > bytes("bytes"), compressed("compressed");
    hls::stream<ap_uint<32> > matches("matches"), filtered("filtered"), boosted("boosted");
    hls::stream<bool> eos("eos");
    hls::stream<uint32_t> sizes("sizes");
#pragma HLS STREAM variable=bytes depth=32
#pragma HLS STREAM variable=compressed depth=32
#pragma HLS STREAM variable=matches depth=32
#pragma HLS STREAM variable=filtered depth=32
#pragma HLS STREAM variable=boosted depth=32
#pragma HLS STREAM variable=eos depth=32
    uint32_t limits[1];
    load_bytes(raw, size, bytes);
    xf::compression::lzCompress<6, 4, 65536, 2>(bytes, matches, size);
    xf::compression::lzBestMatchFilter<6, 65536>(matches, filtered, size);
    xf::compression::lzBooster<255, 32768>(filtered, boosted, size);
    xf::compression::lz4Compress<BLOCK, 1>(boosted, compressed, limits, size, eos, sizes, 0);
    collect(compressed, eos, sizes, encoded, encoded_size);
}

struct ByteWriter {
    hls::stream<Output>& output;
    ap_uint<64> word;
    unsigned count;
    ByteWriter(hls::stream<Output>& out):output(out),word(0),count(0) {}
    void byte(uint8_t v) {
#pragma HLS INLINE
        if (count == 8) flush(false);
        word.range(count*8+7, count*8) = v;
        ++count;
    }
    void u32(uint32_t v) {
        for (unsigned i=0; i<4; ++i) byte(v >> (i*8));
    }
    void flush(bool last) {
#pragma HLS INLINE
        Output out;
        out.data=word; out.keep=(1u<<count)-1; out.strb=out.keep;
        out.last=last; out.user=0; output.write(out);
        word=0; count=0;
    }
};

void compress_ctl(hls::stream<Input>& inStream, hls::stream<Output>& outStream) {
#pragma HLS INTERFACE axis port=inStream
#pragma HLS INTERFACE axis port=outStream
#pragma HLS INTERFACE ap_ctrl_hs port=return
    uint8_t raw[BLOCK], encoded[BLOCK];
#pragma HLS BIND_STORAGE variable=raw type=ram_2p impl=bram
#pragma HLS BIND_STORAGE variable=encoded type=ram_2p impl=bram
    ByteWriter output(outStream);
    // Standard LZ4 frame: independent blocks, maximum block size 64 KiB,
    // no content-size/checksum fields. XXH32(0x60,0x40)>>8 = 0x82.
    output.u32(0x184d2204); output.byte(0x60); output.byte(0x40); output.byte(0x82);
    bool last=false;
    while (!last) {
        unsigned size=0;
        while (size < BLOCK && !last) {
            Input in=inStream.read();
            for (unsigned i=0; i<8; ++i) {
#pragma HLS PIPELINE II=1
                if (in.keep[i]) raw[size++]=in.data.range(i*8+7, i*8);
            }
            last=in.last;
        }
        unsigned compressed_size=size;
        // Upstream LZ77 requires its 64-byte literal tail plus match window.
        if (size > 72) compress_block(raw, size, encoded, compressed_size);
        bool stored=compressed_size>=size;
        output.u32((stored ? 0x80000000u : 0) | (stored ? size : compressed_size));
        for (unsigned i=0; i<(stored ? size : compressed_size); ++i) {
#pragma HLS PIPELINE II=1
            output.byte(stored ? raw[i] : encoded[i]);
        }
    }
    output.u32(0); // EndMark, not an empty data block
    output.flush(true);
}
#else
#error unsupported HLS algorithm
#endif
