// SPDX-License-Identifier: MIT
// One algorithm per image; the 64-bit shell interface is identical in all images.
#include <ap_axi_sdata.h>
#include <hls_stream.h>
#include <stdint.h>

#ifndef CF_ALGORITHM
#error CF_ALGORITHM must be 3 (LZ4), 4 (Zstd) or 5 (MinLZ)
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

static const unsigned BLOCK = 8192;
static const unsigned LANES = 8;

struct InputMeta { unsigned size; bool last; };
struct OutputMeta { unsigned size; unsigned encoded; bool last; };

// Separate the transport into a compression stream and a retained original.
// A whole input block must fit before its size becomes visible to the encoder.
static void read_blocks(hls::stream<Input>& input,
                        hls::stream<ap_uint<64> > compression[LANES],
                        hls::stream<ap_uint<64> >& original,
                        hls::stream<InputMeta> metadata[LANES]) {
    bool last=false;
    unsigned lane=0;
    while (!last) {
        unsigned size=0;
        while (size<BLOCK && !last) {
#pragma HLS PIPELINE II=1
            Input in=input.read();
            compression[lane].write(in.data); original.write(in.data);
            unsigned count=0;
            for (unsigned i=0;i<8;++i) {
#pragma HLS UNROLL
                count+=in.keep[i];
            }
            size+=count;last=in.last;
        }
        InputMeta meta;meta.size=size;meta.last=last;metadata[lane].write(meta);
        lane=(lane+1)%LANES;
    }
    // Every lane terminates independently, including lanes with no input block.
    for (unsigned i=0;i<LANES;++i) {
#pragma HLS PIPELINE II=1
        InputMeta stop;stop.size=0;stop.last=true;metadata[i].write(stop);
    }
}

static void load_bytes(hls::stream<ap_uint<64> >& input, unsigned size,
                       hls::stream<ap_uint<8> >& output) {
    ap_uint<64> word=0;
    for (unsigned i=0;i<size;++i) {
#pragma HLS PIPELINE II=1
        if ((i&7)==0) word=input.read();
        output.write(word.range(7,0)); word>>=8;
    }
}

static void collect(hls::stream<ap_uint<8> >& input, hls::stream<bool>& eos,
                    hls::stream<uint32_t>& sizes, hls::stream<ap_uint<64> >& encoded,
                    unsigned& size) {
    size=0;
    ap_uint<64> word=0;
    unsigned count=0;
    bool end=eos.read();
    while (!end) {
#pragma HLS PIPELINE II=1
        word.range(count*8+7,count*8)=input.read();++count;++size;
        if (count==8) { encoded.write(word);word=0;count=0; }
        end=eos.read();
    }
    if (count) encoded.write(word);
    (void)input.read(); // upstream dummy EOS byte
    (void)sizes.read();
}

static void compress_block(hls::stream<ap_uint<64> >& raw, unsigned size,
                           hls::stream<ap_uint<64> >& encoded, unsigned& encoded_size) {
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
    load_bytes(raw,size,bytes);
    xf::compression::lzCompress<6,4,65536,1>(bytes,matches,size);
    xf::compression::lzBestMatchFilter<6,65536>(matches,filtered,size);
    xf::compression::lzBooster<255,4096>(filtered,boosted,size);
    xf::compression::lz4Compress<BLOCK,1>(boosted,compressed,limits,size,eos,sizes,0);
    collect(compressed,eos,sizes,encoded,encoded_size);
}

static void encode_blocks(hls::stream<ap_uint<64> >& raw,
                          hls::stream<InputMeta>& metadata,
                          hls::stream<ap_uint<64> >& encoded,
                          hls::stream<OutputMeta>& output_metadata) {
    while (true) {
        InputMeta meta=metadata.read();
        if (meta.size==0) break;
        unsigned compressed_size=0;
        if (meta.size>72) {
            compress_block(raw,meta.size,encoded,compressed_size);
        } else {
            // The LZ77 implementation requires its literal tail and match window.
            for (unsigned i=0;i<(meta.size+7)/8;++i) {
#pragma HLS PIPELINE II=1
                (void)raw.read();
            }
        }
        OutputMeta result;result.size=meta.size;result.encoded=compressed_size;
        result.last=meta.last;output_metadata.write(result);
    }
}

// Keep one complete beat pending so an exact 64-bit tail carries TLAST itself.
// Both original and compressed blocks advance eight bytes per cycle.
struct WordWriter {
    hls::stream<Output>& output;
    ap_uint<128> buffer;
    unsigned count;
    WordWriter(hls::stream<Output>& out):output(out),buffer(0),count(0) {}
    void append(ap_uint<64> data,unsigned size) {
#pragma HLS INLINE
        ap_uint<128> mask=(ap_uint<128>(1)<<(size*8))-1;
        buffer|=(ap_uint<128>(data)&mask)<<(count*8);count+=size;
        if (count>8) {
            Output out;out.data=buffer.range(63,0);out.keep=255;out.strb=255;
            out.last=false;out.user=0;output.write(out);buffer>>=64;count-=8;
        }
    }
    void finish() {
#pragma HLS INLINE
        Output out;out.data=buffer.range(63,0);out.keep=(1u<<count)-1;out.strb=out.keep;
        out.last=true;out.user=0;output.write(out);
    }
};

static void write_blocks(hls::stream<ap_uint<64> >& raw,
                         hls::stream<ap_uint<64> > encoded[LANES],
                         hls::stream<OutputMeta> metadata[LANES],
                         hls::stream<Output>& output) {
    WordWriter writer(output);
    writer.append(0x184d2204,4);writer.append(0x824060,3);
    bool last=false;
    unsigned lane=0;
    while (!last) {
        OutputMeta meta=metadata[lane].read();
        bool stored=meta.size<=72 || meta.encoded>=meta.size;
        writer.append((stored?0x80000000u:0)|(stored?meta.size:meta.encoded),4);
        unsigned words=(meta.size+7)/8;
        unsigned encoded_words=(meta.encoded+7)/8;
        unsigned steps=words>encoded_words?words:encoded_words;
        // Drain both copies together. Incompressible blocks must not serialize
        // discarding the attempted encoding and emitting the retained original.
        for (unsigned i=0;i<steps;++i) {
#pragma HLS PIPELINE II=1
            ap_uint<64> original=0,compressed=0;
            if (i<words) original=raw.read();
            if (i<encoded_words) compressed=encoded[lane].read();
            if (stored && i<words) {
                unsigned remaining=meta.size-i*8;
                writer.append(original,remaining<8?remaining:8);
            } else if (!stored && i<encoded_words) {
                unsigned remaining=meta.encoded-i*8;
                writer.append(compressed,remaining<8?remaining:8);
            }
        }
        last=meta.last;
        lane=(lane+1)%LANES;
    }
    writer.append(0,4);writer.finish();
}

void compress_ctl(hls::stream<Input>& inStream,hls::stream<Output>& outStream) {
#pragma HLS INTERFACE axis port=inStream
#pragma HLS INTERFACE axis port=outStream
#pragma HLS INTERFACE ap_ctrl_hs port=return
#pragma HLS DATAFLOW
    hls::stream<ap_uint<64> > compression[LANES],encoded[LANES],original("original");
    hls::stream<InputMeta> input_metadata[LANES];
    hls::stream<OutputMeta> output_metadata[LANES];
    // One complete block per lane is sufficient before size publication. The
    // encoder caps attempted output at BLOCK bytes; EOS adds no payload word.
#pragma HLS STREAM variable=compression depth=1024
#pragma HLS STREAM variable=original depth=8192
#pragma HLS STREAM variable=encoded depth=1024
#pragma HLS STREAM variable=input_metadata depth=4
#pragma HLS STREAM variable=output_metadata depth=4
#pragma HLS BIND_STORAGE variable=compression type=fifo impl=bram
    // Raw retention uses the spare URAM capacity; eight byte-wide compressors
    // otherwise exhaust BRAM when replicated across all four DMA engines.
#pragma HLS BIND_STORAGE variable=original type=fifo impl=uram
#pragma HLS BIND_STORAGE variable=encoded type=fifo impl=bram
    read_blocks(inStream,compression,original,input_metadata);
    for (unsigned i=0;i<LANES;++i) {
#pragma HLS UNROLL
        encode_blocks(compression[i],input_metadata[i],encoded[i],output_metadata[i]);
    }
    write_blocks(original,encoded,output_metadata,outStream);
}

#elif CF_ALGORITHM == 5
#include "minlz_compress.hpp"
#else
#error unsupported HLS algorithm
#endif
