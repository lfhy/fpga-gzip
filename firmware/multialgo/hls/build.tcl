if {[info exists ::env(CF_HLS_ALGORITHM)]} {
    set argv [list $::env(CF_HLS_ALGORITHM) $::env(CF_HLS_BUILD) $::env(CF_HLS_LIBRARY) $::env(CF_HLS_MODE)]
}
if {[llength $argv] != 4} {error "usage: <lz4|zstd> <build-dir> <Vitis_Libraries> <csim|synth>"}
set algorithm [lindex $argv 0]
set root [file normalize [lindex $argv 1]]
set library [file normalize [lindex $argv 2]]
set mode [lindex $argv 3]
set source [file dirname [file normalize [info script]]]
switch $algorithm { lz4 {set id 3} zstd {set id 4} default {error "unsupported algorithm"} }
if {[file isdirectory $library/data_compression]} {set comp $library/data_compression} else {set comp $library/comp}
file mkdir $root
set overlay $root/overrides
file mkdir $overlay
if {$algorithm eq "zstd"} {
    foreach name {zstd_compress_multicore.hpp zstd_compress.hpp zstd_compress_internal.hpp zstd_encoders.hpp} {
        file copy -force $comp/L1/include/hw/$name $overlay/$name
    }
    exec patch -p1 -d $overlay -i $source/zstd-literal-buffer.patch
    exec patch -p1 -d $overlay -i $source/zstd-frame-fse.patch
    exec patch -p1 -d $overlay -i $source/zstd-fifo.patch
}
set flags "-I$overlay -I$comp/L1/include/hw -I$library/security/L1/include -DCF_ALGORITHM=$id -std=c++17"
open_project -reset $root/hls-$algorithm
set_top compress_ctl
add_files $source/compress.cpp -cflags $flags
add_files -tb $source/test.cpp -cflags $flags
open_solution sol1 -flow_target vivado
set_part xcku5p-ffvb676-2-e
create_clock -period 4.0 -name default
config_dataflow -scalar_fifo_depth 32 -start_fifo_depth 32 -task_level_fifo_depth 32
config_export -format ip_catalog -rtl verilog
if {$mode eq "csim"} {
    csim_design -argv $root
} elseif {$mode eq "synth" || $mode eq "cosim"} {
    csynth_design
    if {$mode eq "cosim"} {
        set cases quick
        if {[info exists ::env(CF_HLS_CASES)]} {set cases $::env(CF_HLS_CASES)}
        cosim_design -rtl verilog -argv "$root $cases"
    }
    export_design -rtl verilog -format ip_catalog -output $root/compress_ctl_$algorithm.zip
} else {error "invalid mode"}
exit
