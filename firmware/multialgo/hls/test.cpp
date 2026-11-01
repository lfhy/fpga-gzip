// SPDX-License-Identifier: MIT
#include <ap_axi_sdata.h>
#include <hls_stream.h>
#include <fstream>
#include <iterator>
#include <string>
#include <vector>
#include <stdint.h>
typedef ap_axiu<64,0,0,0> Input;
typedef ap_axiu<64,32,0,0> Output;
void compress_ctl(hls::stream<Input>&, hls::stream<Output>&);

static int check_case(const std::vector<uint8_t>& raw, const std::string& root, unsigned kind) {
    const unsigned size=raw.size();
    hls::stream<Input> in; hls::stream<Output> out;
    for (unsigned i=0;i<size;i+=8) {
        Input v; v.data=0;v.keep=0;v.strb=0;v.last=i+8>=size;
        for(unsigned j=0;j<8&&i+j<size;++j) {v.data.range(j*8+7,j*8)=raw[i+j]; v.keep[j]=1;v.strb[j]=1;}
        in.write(v);
    }
    compress_ctl(in,out);
    std::string name=root+"/case-"+std::to_string(kind)+"-"+std::to_string(size);
    std::ofstream orig(name+".raw", std::ios::binary);orig.write((const char*)raw.data(),size);
    std::ofstream enc(name+".encoded", std::ios::binary);
    bool last=false;
    while(!out.empty()) {
        if(last) return 3;
        Output v=out.read();last=v.last;
        if(v.keep==0) return 4;
        for(unsigned j=0;j<8;++j)if(v.keep[j]) enc.put(v.data.range(j*8+7,j*8).to_uint());
    }
    if(!last||!in.empty()) return 5;
    return 0;
}

int main(int argc, char** argv) {
    if (argc != 2 && argc != 3) return 2;
    if (argc == 3 && std::string(argv[2]) == "dma") {
        for (unsigned size:{32767u,65536u,1048576u}) {
            std::ifstream file(std::string(argv[1])+"/fixtures/case-2-"+std::to_string(size)+".raw",std::ios::binary);
            if(!file) return 6;
            std::vector<uint8_t> raw((std::istreambuf_iterator<char>(file)),{});
            if(raw.size()!=size) return 7;
            int error=check_case(raw,argv[1],2);
            if(error) return error;
        }
        return 0;
    }
    const unsigned sizes[]={1,7,8,9,64,65,71,72,73,127,128,129,255,1023,8191,8192,8193,
#if CF_ALGORITHM == 5
        16383,16384,16385,
#endif
        32767,32768,32769,65536,65537,1048576};
    for (unsigned kind=0; kind<5; ++kind) for (unsigned size:sizes) {
        if (argc == 3 && std::string(argv[2]) == "entropy" && !((kind != 2 && size == 8193) || (kind == 2 && size == 65537))) continue;
        if (argc == 3 && std::string(argv[2]) == "random" && (kind != 2 || (size != 8193 && size != 32767 && size != 65537))) continue;
        if (argc == 3 && size != 1 && size != 73 && size != 8193 && size != 32767 && size != 65537) continue;
        std::vector<uint8_t> raw(size);
        uint32_t rng=0x12345678;
        for (unsigned i=0;i<size;++i) {
            rng^=rng<<13; rng^=rng>>17; rng^=rng<<5;
            raw[i]=kind==0 ? 0 : kind==1 ? "The quick brown fox jumps over the lazy dog.\n"[i%44] :
                   kind==2 ? rng : kind==3 ? 'A' : i%2;
        }
        int error=check_case(raw,argv[1],kind);
        if(error) return error;
    }
    return 0;
}
