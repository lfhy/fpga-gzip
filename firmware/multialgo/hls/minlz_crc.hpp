// SPDX-License-Identifier: MIT
// Parallel CRC32C transform for eight input bytes, reflected polynomial
// 0x82f63b78. Each output is a balanced parity tree, not 64 serial bit steps.
#pragma once
static uint32_t minlz_crc64(uint32_t previous, ap_uint<64> word) {
#pragma HLS INLINE
    static const uint64_t data_mask[32]={
        0x1f91caf6386f2c25ULL,
        0x3f2395ec70de584bULL,
        0x7e472bd8e1bcb097ULL,
        0xfc8e57b1c379612eULL,
        0xe68d6595be9dee78ULL,
        0xd28b01dd4554f0d5ULL,
        0xba87c94cb2c6cd8eULL,
        0x6a9e586f5de2b738ULL,
        0xd53cb0debbc56e71ULL,
        0xb5e8ab4b4fe5f0c6ULL,
        0x74409c60a7a4cda9ULL,
        0xe88138c14f499b53ULL,
        0xce93bb74a6fc1a83ULL,
        0x82b6bc1f75971923ULL,
        0x1afcb2c8d3411e63ULL,
        0x35f96591a6823cc6ULL,
        0x6bf2cb234d04798cULL,
        0xd7e596469a08f318ULL,
        0xb05ae67b0c7eca15ULL,
        0x7f2406002092b80fULL,
        0xfe480c004125701fULL,
        0xe301d2f6ba25cc1bULL,
        0xd9926f1b4c24b412ULL,
        0xacb514c0a0264400ULL,
        0x46fbe3777823a425ULL,
        0x8df7c6eef047484aULL,
        0x047e472bd8e1bcb0ULL,
        0x08fc8e57b1c37961ULL,
        0x11f91caf6386f2c2ULL,
        0x23f2395ec70de584ULL,
        0x47e472bd8e1bcb09ULL,
        0x8fc8e57b1c379612ULL};
    static const uint32_t crc_mask[32]={
        0x386f2c25u,
        0x70de584bu,
        0xe1bcb097u,
        0xc379612eu,
        0xbe9dee78u,
        0x4554f0d5u,
        0xb2c6cd8eu,
        0x5de2b738u,
        0xbbc56e71u,
        0x4fe5f0c6u,
        0xa7a4cda9u,
        0x4f499b53u,
        0xa6fc1a83u,
        0x75971923u,
        0xd3411e63u,
        0xa6823cc6u,
        0x4d04798cu,
        0x9a08f318u,
        0x0c7eca15u,
        0x2092b80fu,
        0x4125701fu,
        0xba25cc1bu,
        0x4c24b412u,
        0xa0264400u,
        0x7823a425u,
        0xf047484au,
        0xd8e1bcb0u,
        0xb1c37961u,
        0x6386f2c2u,
        0xc70de584u,
        0x8e1bcb09u,
        0x1c379612u};
    ap_uint<32> result=0;
    for (unsigned bit=0;bit<32;++bit) {
#pragma HLS UNROLL
        ap_uint<64> data_bits=word&data_mask[bit];
        ap_uint<32> crc_bits=previous&crc_mask[bit];
        result[bit]=data_bits.xor_reduce()^crc_bits.xor_reduce();
    }
    return result.to_uint();
}
static uint32_t minlz_crc_byte(uint32_t crc, uint8_t byte) {
#pragma HLS INLINE
    crc^=byte;
    for (unsigned bit=0;bit<8;++bit) {
#pragma HLS UNROLL
        crc=(crc>>1)^((crc&1)?0x82f63b78u:0);
    }
    return crc;
}
