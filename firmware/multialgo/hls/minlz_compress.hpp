// SPDX-License-Identifier: MIT
// This implements the MinLZ specification v1.0.
// Encoder subset: 16 KiB independent blocks, aligned 8-byte matches, literals,
// Copy1, Copy2 and repeats; type 1/2 streams with CRC32C and length-bearing EOF.
// Matching policy is independent of the format and intentionally favors speed.
#pragma once
#include "minlz_crc.hpp"
namespace cf_minlz {
static const unsigned BLOCK=16384;
static const unsigned WORDS=BLOCK/8;
static const unsigned LANES=4;
static const unsigned HASH=1024;
struct InputMeta { unsigned size; uint32_t crc; bool last; };
struct OutputMeta { unsigned size; unsigned encoded; uint32_t crc; bool last; };

static void read_blocks(hls::stream<Input>& input,
                        hls::stream<ap_uint<64> > compression[LANES],
                        hls::stream<ap_uint<64> >& original,
                        hls::stream<InputMeta> metadata[LANES]) {
    bool last=false;
    unsigned lane=0;
    while (!last) {
        unsigned size=0,tail_count=0;
        ap_uint<64> tail=0;
        uint32_t crc=0xffffffffu;
        while (size<BLOCK && !last) {
#pragma HLS PIPELINE II=1
            Input in=input.read();
            compression[lane].write(in.data);original.write(in.data);
            unsigned count=0;
            for (unsigned i=0;i<8;++i) {
#pragma HLS UNROLL
                count+=in.keep[i];
            }
            if (count==8) crc=minlz_crc64(crc,in.data);
            else {tail=in.data;tail_count=count;}
            size+=count;last=in.last;
        }
        // Only the final beat can be partial. Keep its bytewise CRC work out
        // of the full-width read loop; at most seven additional clocks.
        for (unsigned i=0;i<tail_count;++i) {
#pragma HLS PIPELINE II=1
            crc=minlz_crc_byte(crc,tail.range(7,0));tail>>=8;
        }
        crc=~crc;
        InputMeta meta;meta.size=size;meta.crc=((crc>>15)|(crc<<17))+0xa282ead8u;
        meta.last=last;metadata[lane].write(meta);
        lane=(lane+1)%LANES;
    }
    for (unsigned i=0;i<LANES;++i) {
#pragma HLS PIPELINE II=1
        InputMeta stop;stop.size=0;stop.crc=0;stop.last=true;metadata[i].write(stop);
    }
}

// Attempted encoding is capped at the original block size. Reaching the cap
// always selects the retained original, so truncated attempts cannot escape.
// The entire attempt fits its FIFO before its metadata is published.
struct AttemptWriter {
    hls::stream<ap_uint<64> >& output;
    ap_uint<64> buffer;
    ap_uint<3> count;
    ap_uint<15> size,limit;
    AttemptWriter(hls::stream<ap_uint<64> >& out,unsigned cap):
        output(out),buffer(0),count(0),size(0),limit(cap) {}
    void append(ap_uint<64> data,ap_uint<4> n) {
#pragma HLS INLINE
        ap_uint<15> room=limit-size;
        if (n>room) n=room;
        if (n==0) return;
        ap_uint<64> mask=~ap_uint<64>(0);
        mask>>=(ap_uint<4>(8)-n)*8;
        ap_uint<128> packed=(ap_uint<128>(data&mask)<<(count*8))|buffer;
        ap_uint<4> next=ap_uint<4>(count)+n;
        if (next>=8) {
            output.write(packed.range(63,0));buffer=packed.range(127,64);count=next-8;
        } else {buffer=packed.range(63,0);count=next;}
        size+=n;
    }
    void finish() {
#pragma HLS INLINE
        if (count) output.write(buffer);
    }
};
struct Token { ap_uint<64> data; ap_uint<4> size; };
static Token length_token(unsigned size,bool repeat) {
#pragma HLS INLINE
    Token token;
    if (size<=29) {token.data=(size-1)<<3;token.size=1;}
    else if (size<=285) {token.data=(29u<<3)|((size-30)<<8);token.size=2;}
    else {token.data=(30u<<3)|((size-30)<<8);token.size=3;}
    if (repeat) token.data|=4;
    return token;
}
static Token copy_token(unsigned offset,unsigned length,unsigned& used) {
#pragma HLS INLINE
    Token token;used=length;
    if (offset<=1024) {
        used=length>273?273:length;
        unsigned stored=offset-1;
        uint32_t value=1u|((stored&3u)<<6)|((stored>>2)<<8);
        if (used<=18) {token.data=value|((used-4)<<2);token.size=2;}
        else {token.data=value|(15u<<2)|((used-18)<<16);token.size=3;}
    } else {
        uint32_t value=2u|((offset-64)<<8);
        if (length<=64) {token.data=value|((length-4)<<2);token.size=3;}
        else if (length<=319) {token.data=value|(61u<<2)|((length-64)<<24);token.size=4;}
        else {token.data=ap_uint<64>(value|(62u<<2))|(ap_uint<64>(length-64)<<24);token.size=5;}
    }
    return token;
}

static unsigned hash_word(ap_uint<64> word) {
#pragma HLS INLINE
    // Folding uses all bytes and avoids a large multiplier on the feedback
    // path. Every candidate is checked against all 64 bits before copying.
    ap_uint<64> mixed=word^(word>>23)^(word>>41);
    return (mixed^(mixed>>10)^(mixed>>20)^(mixed>>30)).range(9,0).to_uint();
}

static void compress_block(hls::stream<ap_uint<64> >& input,unsigned size,
                           hls::stream<ap_uint<64> >& output,unsigned& encoded_size) {
    ap_uint<80> dictionary[HASH];
    ap_uint<64> raw[WORDS];
    ap_uint<32> runs[WORDS];
#pragma HLS BIND_STORAGE variable=dictionary type=ram_t2p impl=bram
#pragma HLS BIND_STORAGE variable=raw type=ram_2p impl=bram
#pragma HLS BIND_STORAGE variable=runs type=ram_2p impl=bram
    for (unsigned i=0;i<HASH;++i) {
#pragma HLS PIPELINE II=1
        dictionary[i]=0;
    }
    unsigned run_count=0,run_offset=0,run_size=0;
    for (unsigned i=0;i<(size+7)/8;++i) {
#pragma HLS PIPELINE II=1
        ap_uint<64> word=input.read();raw[i]=word;
        unsigned bytes=size-i*8; if (bytes>8) bytes=8;
        unsigned hash=hash_word(word);
        ap_uint<80> entry=dictionary[hash];
        unsigned position=entry.range(79,64).to_uint();
        unsigned offset=0;
        if (bytes==8 && position!=0 && entry.range(63,0)==word) offset=(i+1-position)*8;
        ap_uint<80> update=word;update.range(79,64)=i+1;dictionary[hash]=update;
        if (run_size && offset!=run_offset) {
            runs[run_count++]=(run_offset<<16)|run_size;run_size=0;
        }
        run_offset=offset;run_size+=bytes;
    }
    if (run_size) runs[run_count++]=(run_offset<<16)|run_size;

    AttemptWriter writer(output,size);
    // Stream type 2 omits the block's initial MinLZ zero identifier.
    uint32_t varint=(size&127)|128u;
    if (size<128) writer.append(size,1);
    else if (size<16384) writer.append(varint|((size>>7)<<8),2);
    else writer.append(varint|(((size>>7)&127|128u)<<8)|((size>>14)<<16),3);
    unsigned position=0,previous=1;
    for (unsigned i=0;i<run_count;++i) {
        ap_uint<32> run=runs[i];
        unsigned length=run.range(15,0).to_uint();
        unsigned offset=run.range(31,16).to_uint();
        unsigned used=length;
        Token token=offset==0?length_token(length,false):
                    offset==previous?length_token(length,true):copy_token(offset,length,used);
        writer.append(token.data,token.size);
        if (offset==0) {
            for (unsigned byte=0;byte<length;byte+=8) {
#pragma HLS PIPELINE II=1
                unsigned count=length-byte; if (count>8) count=8;
                writer.append(raw[(position+byte)/8],count);
            }
        } else {
            if (used<length) {
                Token repeat=length_token(length-used,true);
                writer.append(repeat.data,repeat.size);
            }
            previous=offset;
        }
        position+=length;
    }
    writer.finish();encoded_size=writer.size;
}

static void encode_blocks(hls::stream<ap_uint<64> >& input,
                          hls::stream<InputMeta>& metadata,
                          hls::stream<ap_uint<64> >& encoded,
                          hls::stream<OutputMeta>& output_metadata) {
    while (true) {
        InputMeta meta=metadata.read();if (meta.size==0) break;
        unsigned compressed_size=0;
        compress_block(input,meta.size,encoded,compressed_size);
        OutputMeta result;result.size=meta.size;result.encoded=compressed_size;
        result.crc=meta.crc;result.last=meta.last;output_metadata.write(result);
    }
}

struct StreamWriter {
    hls::stream<Output>& output;
    ap_uint<64> buffer,pending;
    ap_uint<3> count;
    bool valid;
    StreamWriter(hls::stream<Output>& out):output(out),buffer(0),pending(0),count(0),valid(false) {}
    void emit(ap_uint<64> data,ap_uint<4> bytes,bool last) {
#pragma HLS INLINE
        Output out;out.data=data;out.keep=(1u<<bytes)-1;out.strb=out.keep;
        out.last=last;out.user=0;output.write(out);
    }
    void append(ap_uint<64> data,ap_uint<4> bytes) {
#pragma HLS INLINE
        ap_uint<64> mask=~ap_uint<64>(0);mask>>=(ap_uint<4>(8)-bytes)*8;
        ap_uint<128> packed=(ap_uint<128>(data&mask)<<(count*8))|buffer;
        ap_uint<4> next=ap_uint<4>(count)+bytes;
        if (next>=8) {
            if (valid) emit(pending,8,false);
            pending=packed.range(63,0);valid=true;
            buffer=packed.range(127,64);count=next-8;
        } else {buffer=packed.range(63,0);count=next;}
    }
    void finish() {
#pragma HLS INLINE
        if (count) {
            if (valid) emit(pending,8,false);
            emit(buffer,count,true);
        } else emit(pending,8,true);
    }
};

static void write_blocks(hls::stream<ap_uint<64> >& raw,
                         hls::stream<ap_uint<64> > encoded[LANES],
                         hls::stream<OutputMeta> metadata[LANES],
                         hls::stream<Output>& output) {
    StreamWriter writer(output);
    writer.append(0x000006ffu,4);writer.append(0x047a4c6e694dULL,6);
    bool last=false;unsigned lane=0;uint64_t total=0;
    while (!last) {
        OutputMeta meta=metadata[lane].read();
        bool stored=meta.encoded>=meta.size;
        unsigned payload=stored?meta.size:meta.encoded;
        writer.append(((payload+4)<<8)|(stored?1u:2u),4);writer.append(meta.crc,4);
        unsigned words=(meta.size+7)/8,encoded_words=(meta.encoded+7)/8;
        unsigned steps=words>encoded_words?words:encoded_words;
        for (unsigned i=0;i<steps;++i) {
#pragma HLS PIPELINE II=1
            ap_uint<64> original=0,compressed=0;
            if (i<words) original=raw.read();
            if (i<encoded_words) compressed=encoded[lane].read();
            if ((stored && i<words)||(!stored && i<encoded_words)) {
                unsigned remaining=payload-i*8;
                writer.append(stored?original:compressed,remaining<8?remaining:8);
            }
        }
        total+=meta.size;last=meta.last;lane=(lane+1)%LANES;
    }
    ap_uint<80> varint=0;unsigned bytes=0;
    do {
        uint8_t value=total&127;total>>=7;if (total) value|=128;
        varint.range(bytes*8+7,bytes*8)=value;++bytes;
    } while (total);
    writer.append((bytes<<8)|0x20u,4);
    writer.append(varint.range(63,0),bytes>8?8:bytes);
    if (bytes>8) writer.append(varint.range(79,64),bytes-8);
    writer.finish();
}
} // namespace cf_minlz

void compress_ctl(hls::stream<Input>& inStream,hls::stream<Output>& outStream) {
#pragma HLS INTERFACE axis port=inStream
#pragma HLS INTERFACE axis port=outStream
#pragma HLS INTERFACE ap_ctrl_hs port=return
#pragma HLS DATAFLOW
    using namespace cf_minlz;
    hls::stream<ap_uint<64> > compression[LANES],encoded[LANES],original("original");
    hls::stream<InputMeta> input_metadata[LANES];
    hls::stream<OutputMeta> output_metadata[LANES];
#pragma HLS STREAM variable=compression depth=2048
#pragma HLS STREAM variable=encoded depth=2048
#pragma HLS STREAM variable=original depth=8192
#pragma HLS STREAM variable=input_metadata depth=4
#pragma HLS STREAM variable=output_metadata depth=4
#pragma HLS BIND_STORAGE variable=compression type=fifo impl=bram
#pragma HLS BIND_STORAGE variable=encoded type=fifo impl=bram
#pragma HLS BIND_STORAGE variable=original type=fifo impl=uram
    read_blocks(inStream,compression,original,input_metadata);
    for (unsigned i=0;i<LANES;++i) {
#pragma HLS UNROLL
        encode_blocks(compression[i],input_metadata[i],encoded[i],output_metadata[i]);
    }
    write_blocks(original,encoded,output_metadata,outStream);
}
