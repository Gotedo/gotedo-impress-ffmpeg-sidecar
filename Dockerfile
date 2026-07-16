# syntax=docker/dockerfile:1

FROM rust:1.93.1-slim-bookworm

RUN echo 'Acquire::HTTP::Proxy "http://host.docker.internal:3142";' >> /etc/apt/apt.conf.d/01proxy \
  && echo 'Acquire::HTTPS::Proxy "false";' >> /etc/apt/apt.conf.d/01proxy

# Install prerequisites to fetch external secure repositories
RUN apt-get update && apt-get install -y --no-install-recommends \
  wget ca-certificates gnupg \
  && rm -rf /var/lib/apt/lists/*

# Add the official LLVM 20 repository key and source lists
RUN wget -qO- https://apt.llvm.org/llvm-snapshot.gpg.key | tee /etc/apt/trusted.gpg.d/apt.llvm.org.asc && \
  echo "deb http://apt.llvm.org/bookworm/ llvm-toolchain-bookworm-20 main" >> /etc/apt/sources.list && \
  echo "deb-src http://apt.llvm.org/bookworm/ llvm-toolchain-bookworm-20 main" >> /etc/apt/sources.list

# -----------------------------------------------------------------------------
# Base packages + FFmpeg-specific build tools
# -----------------------------------------------------------------------------
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
  --mount=type=cache,target=/var/lib/apt,sharing=locked \
  apt-get update && apt-get install -y --no-install-recommends \
  # Cross compilers & toolchains (Linux & Windows)
  gcc-mingw-w64-x86-64 g++-mingw-w64-x86-64 \
  gcc-aarch64-linux-gnu g++-aarch64-linux-gnu \
  gcc-x86-64-linux-gnu g++-x86-64-linux-gnu \
  mingw-w64 \
  # Native Base Utilities
  git make cmake curl xz-utils bzip2 unzip python3-pip ninja-build pkg-config \
  bison flex gperf gettext libglib2.0-dev file binfmt-support \
  # Explicit LLVM 20 Toolchain & Darwin Compiler Runtime (Mandatory)
  clang-20 \
  lld-20 \
  llvm-20 \
  llvm-20-linker-tools \
  llvm-20-dev \
  libclang-rt-20-dev \
  # Windows Execution Compatibility
  wine wine64 libwine \
  # FFmpeg-specific Assemblers & Build Tools
  nasm yasm ccache autoconf automake libtool texinfo \
  libfreetype6-dev libharfbuzz-dev \
  # Debugging Tools
  nano vim \
  && rm -rf /var/lib/apt/lists/*

# Install Go Toolchain for Sidecar Compilation
ARG GO_VERSION=1.26.0
RUN ARCH_SUFFIX=$(dpkg --print-architecture) && \
  curl -L "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH_SUFFIX}.tar.gz" | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:${PATH}"

# Generate Global LLVM 20 System Symlinks (Overriding default LLVM 14)
RUN ln -sf /usr/bin/clang-20 /usr/bin/clang && \
  ln -sf /usr/bin/clang++-20 /usr/bin/clang++ && \
  ln -sf /usr/bin/lld-20 /usr/bin/lld && \
  ln -sf /usr/bin/ld.lld-20 /usr/bin/ld.lld && \
  ln -sf /usr/bin/llvm-ar-20 /usr/bin/llvm-ar && \
  ln -sf /usr/bin/llvm-ranlib-20 /usr/bin/llvm-ranlib && \
  ln -sf /usr/bin/llvm-nm-20 /usr/bin/llvm-nm && \
  ln -sf /usr/bin/llvm-strip-20 /usr/bin/llvm-strip && \
  ln -sf /usr/bin/llvm-strings-20 /usr/bin/llvm-strings

# Install Meson (needed for dav1d, harfbuzz, etc.)
RUN pip3 install meson --break-system-packages

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
  --mount=type=cache,target=/var/lib/apt,sharing=locked \
  apt-get update && apt-get install -y patchelf && rm -rf /var/lib/apt/lists/*

# LLVM tool symlinks
RUN ln -s /usr/bin/llvm-install-name-tool-$(clang --version | grep -oE '[0-9]+' | head -1) /usr/bin/llvm-install-name-tool && \
  ln -s /usr/bin/llvm-otool-$(clang --version | grep -oE '[0-9]+' | head -1) /usr/bin/llvm-otool && \
  ln -s /usr/bin/llvm-lipo-$(clang --version | grep -oE '[0-9]+' | head -1) /usr/bin/lipo

# -----------------------------------------------------------------------------
# Cache & Install LLVM-Mingw (Windows Toolchain) - identical caching pattern
# -----------------------------------------------------------------------------
RUN --mount=type=cache,target=/var/cache/llvm-mingw <<EOF
#!/bin/bash
set -e
ARCH_SUFFIX=$(uname -m)
[ "$ARCH_SUFFIX" = "x86_64" ] && URL_ARCH="x86_64" || URL_ARCH="aarch64"
LLVM_MINGW_URL="https://github.com/mstorsjo/llvm-mingw/releases/download/20260127/llvm-mingw-20260127-ucrt-ubuntu-22.04-${URL_ARCH}.tar.xz"
CACHE_FILE="/var/cache/llvm-mingw/llvm-mingw-${URL_ARCH}.tar.xz"
INSTALL_DIR="/opt/llvm-mingw/${URL_ARCH}"
mkdir -p /opt/llvm-mingw
if [ ! -f "$CACHE_FILE" ]; then
    echo "Downloading llvm-mingw to cache..."
    curl -L "$LLVM_MINGW_URL" -o "$CACHE_FILE"
else
    echo "Using cached llvm-mingw."
fi
if [ ! -d "$INSTALL_DIR" ]; then
    echo "Extracting llvm-mingw..."
    tar -xJf "$CACHE_FILE" -C /opt/llvm-mingw
    mv /opt/llvm-mingw/llvm-mingw-* "$INSTALL_DIR"
fi
EOF

ARG MACOS_SDK_VERSION=14.5

# -----------------------------------------------------------------------------
# Cache & Install MacOS SDK - identical caching pattern
# -----------------------------------------------------------------------------
RUN --mount=type=cache,target=/var/cache/macosx-sdk <<EOF
#!/bin/bash
set -e
SDK_URL="https://github.com/joseluisq/macosx-sdks/releases/download/${MACOS_SDK_VERSION}/MacOSX${MACOS_SDK_VERSION}.sdk.tar.xz"
CACHE_FILE="/var/cache/macosx-sdk/MacOSX${MACOS_SDK_VERSION}.sdk.tar.xz"
INSTALL_DIR="/opt/macos-sdk"
if [ ! -f "$CACHE_FILE" ]; then
    echo "Downloading MacOS SDK to cache..."
    curl -L "$SDK_URL" -o "$CACHE_FILE"
else
    echo "Using cached MacOS SDK."
fi
if [ ! -d "$INSTALL_DIR" ]; then
    echo "Extracting MacOS SDK..."
    mkdir -p /opt
    tar -xJf "$CACHE_FILE" -C /opt
    mv "/opt/MacOSX${MACOS_SDK_VERSION}.sdk" "$INSTALL_DIR"
fi
EOF

# Install patchelf for Linux relocatable SDK patching
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
  --mount=type=cache,target=/var/lib/apt,sharing=locked \
  apt-get update && apt-get install -y patchelf \
  && rm -rf /var/lib/apt/lists/*

# -----------------------------------------------------------------------------
# Cache & Build Apple compiler-rt (builtins) for macOS cross-compilation
# -----------------------------------------------------------------------------
RUN --mount=type=cache,target=/var/cache/compiler-rt <<'EOF'
#!/bin/bash
set -e

# Extract the exact LLVM version installed on the host (e.g., 20.1.8)
CLANG_FULL_VER=$(clang --version | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)

# Downstream files we need to download
LLVM_URL="https://github.com/llvm/llvm-project/releases/download/llvmorg-${CLANG_FULL_VER}/compiler-rt-${CLANG_FULL_VER}.src.tar.xz"
CMAKE_URL="https://github.com/llvm/llvm-project/releases/download/llvmorg-${CLANG_FULL_VER}/cmake-${CLANG_FULL_VER}.src.tar.xz"

CACHE_FILE="/var/cache/compiler-rt/compiler-rt-${CLANG_FULL_VER}.src.tar.xz"
CMAKE_CACHE_FILE="/var/cache/compiler-rt/cmake-${CLANG_FULL_VER}.src.tar.xz"

# Locate Clang's internal resource library path
RESOURCE_DIR=$(/usr/bin/clang -print-resource-dir)
DARWIN_LIB_DIR="${RESOURCE_DIR}/lib/darwin"
COMPILER_RT_ARCHIVE="${RESOURCE_DIR}/lib/darwin/libclang_rt.builtins_osx.a"

# 1. Ensure cache directories and source downloads are resolved
mkdir -p /var/cache/compiler-rt

if [ ! -f "$CACHE_FILE" ]; then
    echo "Downloading compiler-rt ${CLANG_FULL_VER} source..."
    curl -L "$LLVM_URL" -o "$CACHE_FILE"
fi

if [ ! -f "$CMAKE_CACHE_FILE" ]; then
    echo "Downloading LLVM CMake modules companion..."
    curl -L "$CMAKE_URL" -o "$CMAKE_CACHE_FILE"
fi

if [ ! -f "${COMPILER_RT_ARCHIVE}" ]; then
    echo "Building macOS compiler-rt builtins..."
    rm -rf /var/cache/compiler-rt/src
    mkdir -p /var/cache/compiler-rt/src
    
    # Extract compiler-rt source code
    tar -xJf "$CACHE_FILE" -C /var/cache/compiler-rt/src
    # Extract the shared CMake support folders (Fixes 'ExtendPath.cmake' missing error)
    tar -xJf "$CMAKE_CACHE_FILE" -C /var/cache/compiler-rt/src
    
    # Re-route to target source directory
    cd /var/cache/compiler-rt/src/compiler-rt-${CLANG_FULL_VER}.src
    
    # MERGE global LLVM CMake modules into compiler-rt's private Modules folder.
    # This preserves private local modules like BuiltinTests and CompilerRTDarwinUtils!
    mkdir -p cmake/Modules
    cp -r /var/cache/compiler-rt/src/cmake-${CLANG_FULL_VER}.src/Modules/* cmake/Modules/
    
    # Configure CMake to build x86_64 and arm64 Apple Builtins
    mkdir build || true
    cd build

    # FORCE arm64/x86_64 support by patching the SDK check in compiler-rt
    COMPILER_RT_IX_FILE="/var/cache/compiler-rt/src/compiler-rt-${CLANG_FULL_VER}.src/cmake/builtin-config-ix.cmake"
    if [ -f "${COMPILER_RT_IX_FILE}" ]; then
        echo ">>> Patching compiler-rt SDK check to force arm64 support..."
        sed -i '/function(sdk_has_arch_support/,/^  endfunction()/c\
function(sdk_has_arch_support sdk_path os arch has_support)\n  set("${has_support}" On PARENT_SCOPE)\nendfunction()' "${COMPILER_RT_IX_FILE}"
    else
        echo ">>> ERROR: Could not find ${COMPILER_RT_IX_FILE} to patch!"
        exit 1
    fi
    
    echo ">>> Running CMake configuration..."
    if ! cmake ../lib/builtins \
        -G Ninja \
        -DCMAKE_C_COMPILER=clang \
        -DCMAKE_CXX_COMPILER=clang++ \
        -DCMAKE_ASM_COMPILER=clang \
        -DCMAKE_SYSTEM_NAME=Darwin \
        -DCMAKE_OSX_SYSROOT=/opt/macos-sdk \
        -DCMAKE_OSX_ARCHITECTURES="x86_64;arm64" \
        -DDARWIN_osx_ARCHS="x86_64;arm64" \
        -DDARWIN_macosx_OVERRIDE_SDK_VERSION="${MACOS_SDK_VERSION}" \
        -DCMAKE_AR=/usr/bin/llvm-ar \
        -DCMAKE_RANLIB=/usr/bin/llvm-ranlib \
        -DCOMPILER_RT_DEFAULT_TARGET_ONLY=ON \
        -DCOMPILER_RT_ENABLE_IOS=OFF \
        -DCOMPILER_RT_ENABLE_WATCHOS=OFF \
        -DCOMPILER_RT_ENABLE_TVOS=OFF \
        -DCMAKE_BUILD_TYPE=Release \
        -DCMAKE_C_COMPILER_TARGET="x86_64-apple-darwin" \
        -DCMAKE_ASM_COMPILER_TARGET="x86_64-apple-darwin" \
        -DDARWIN_macosx_CACHED_SYSROOT=/opt/macos-sdk \
        -DDARWIN_macosx_SYSROOT=/opt/macos-sdk \
        -DCOMPILER_RT_STANDALONE_BUILD=ON \
        -DLLVM_MAIN_SRC_DIR=/var/cache/compiler-rt/src/compiler-rt-${CLANG_FULL_VER}.src; then
        
        echo "=================================================="
        echo " ERROR: CMake configuration failed! "
        echo "=================================================="
        echo "--- Dumping CMakeOutput.log ---"
        cat CMakeFiles/CMakeOutput.log || echo "CMakeOutput.log not found."
        
        echo -e "\n--- Dumping CMakeError.log ---"
        cat CMakeFiles/CMakeError.log || echo "CMakeError.log not found."
        echo "=================================================="
        
        # Restore plist if configuration fails
        [ -f "/opt/macos-sdk/SDKSettings.plist.bak" ] && mv /opt/macos-sdk/SDKSettings.plist.bak /opt/macos-sdk/SDKSettings.plist
        exit 1
    fi
        
    echo ">>> Running Ninja build..."
    if ! ninja; then
        [ -f "/opt/macos-sdk/SDKSettings.plist.bak" ] && mv /opt/macos-sdk/SDKSettings.plist.bak /opt/macos-sdk/SDKSettings.plist
        echo "=================================================="
        echo " ERROR: Ninja compilation failed! "
        echo "=================================================="
        exit 1
    fi

    # Restore plist after successful compilation
    if [ -f "/opt/macos-sdk/SDKSettings.plist.bak" ]; then
        mv /opt/macos-sdk/SDKSettings.plist.bak /opt/macos-sdk/SDKSettings.plist
    fi
    
    # Inject the resulting fat library into the host Clang
    mkdir -p "${DARWIN_LIB_DIR}"
    
    # Copy and rename the archive to the target filename expected by your builder script
    if find lib/darwin -name "libclang_rt.osx.a" -exec cp {} "${COMPILER_RT_ARCHIVE}" \; ; then
        echo ">>> SUCCESS: libclang_rt.osx.a copied to ${COMPILER_RT_ARCHIVE}"
        
        # ---------------------------------------------------------------------
        # Architecture Verification Safeguard
        # ---------------------------------------------------------------------
        echo ">>> Verifying architecture slices in ${COMPILER_RT_ARCHIVE}..."
        
        # Get file metadata
        LIB_INFO=$(file "${COMPILER_RT_ARCHIVE}")
        echo "Metadata: ${LIB_INFO}"
        
        # Verify both x86_64 and arm64 slices exist
        if echo "${LIB_INFO}" | grep -q "x86_64" && echo "${LIB_INFO}" | grep -q "arm64"; then
            echo ">>> SUCCESS: Verified compiler-rt builtins contain both x86_64 and arm64 slices!"
        else
            echo "====================================================================="
            echo " ERROR: Architecture verification failed! "
            echo " Builtin library does not contain both x86_64 and arm64 slices. "
            echo " Found: ${LIB_INFO}"
            echo "====================================================================="
            exit 1
        fi
        # ---------------------------------------------------------------------
        
    else
        echo ">>> ERROR: Failed to locate or copy libclang_rt.osx.a to ${COMPILER_RT_ARCHIVE}!"
        exit 1
    fi
    
    rm -rf /var/cache/compiler-rt/src
    echo ">>> macOS compiler-rt successfully injected into host Clang."
else
    echo ">>> macOS compiler-rt builtins already installed."
fi
EOF

# Install gas-preprocessor for macOS assembly optimization parsing
RUN curl -L https://github.com/FFmpeg/gas-preprocessor/raw/master/gas-preprocessor.pl -o /usr/local/bin/gas-preprocessor.pl && \
  chmod +x /usr/local/bin/gas-preprocessor.pl

# -----------------------------------------------------------------------------
# The main builder script
# -----------------------------------------------------------------------------
RUN cat <<'BUILDER' > /usr/local/bin/builder.sh
#!/bin/bash
set -e

# -----------------------------------------------------------------------------
# Configuration - FFmpeg 8.1.2 + all required dependencies
# -----------------------------------------------------------------------------
FFMPEG_VERSION="8.1.2"

# Dependency versions (chosen for stability + cross-compile friendliness in 2026)
ZLIB_VERSION="1.3.2"
LIBPNG_VERSION="1.6.43"
LIBJPEG_TURBO_VERSION="3.0.4"
LIBWEBP_VERSION="1.4.0"
TIFF_VERSION="4.6.0"
FREETYPE_VERSION="2.14.3"
HARFBUZZ_VERSION="9.0.0"
BROTLI_VERSION="1.2.0"
LIBDE265_VERSION="1.1.1"
FRIBIDI_VERSION="1.0.15"
EXPAT_VERSION="2.6.2"
FONTCONFIG_VERSION="2.15.0"
LIBASS_VERSION="0.17.3"
OPUS_VERSION="1.5.2"
LIBVPX_VERSION="1.14.1"
X264_VERSION="stable"          # videolan stable branch tarball
X265_VERSION="3.6"
DAV1D_VERSION="1.4.3"
LAME_VERSION="4.0"
LIBAOM_VERSION="3.11.0"
LIBVORBIS_VERSION="1.3.7"
LIBOGG_VERSION="1.3.5"
BZIP2_VERSION="1.0.8"
XZ_VERSION="5.8.3"
MINIAUDIO_VERSION="0.11.25"

DEP_LIBRARY_TYPE="static"      # Sidecar is open-sourced for GPL compliance so libraries can be statically built

# Map DEP_LIBRARY_TYPE to build flags
if [ "$DEP_LIBRARY_TYPE" = "shared" ]; then
    AUTO_CONF_FLAGS="--disable-static --enable-shared"
    CMAKE_CONF_FLAGS="-DBUILD_SHARED_LIBS=ON -DENABLE_SHARED=ON -DENABLE_STATIC=OFF -DCMAKE_POSITION_INDEPENDENT_CODE=ON -DCMAKE_C_COMPILER_LAUNCHER=ccache -DCMAKE_CXX_COMPILER_LAUNCHER=ccache -DCMAKE_INSTALL_LIBDIR=lib"
    MESON_CONF_FLAGS="--default-library=shared"
    # FFmpeg uses its own flags below
else
    AUTO_CONF_FLAGS="--enable-static --disable-shared"
    CMAKE_CONF_FLAGS="-DBUILD_SHARED_LIBS=OFF -DENABLE_SHARED=OFF -DENABLE_STATIC=ON -DCMAKE_POSITION_INDEPENDENT_CODE=ON -DCMAKE_C_COMPILER_LAUNCHER=ccache -DCMAKE_CXX_COMPILER_LAUNCHER=ccache -DCMAKE_INSTALL_LIBDIR=lib"
    MESON_CONF_FLAGS="--default-library=static"
fi

SDK_PATH="/opt/macos-sdk"
LLVM_MINGW_PATH="/opt/llvm-mingw/$(uname -m)"
export PATH="$LLVM_MINGW_PATH/bin:${PATH}"
FFMPEG_OS_FLAGS=""
EXTRA_FFMPEG_FLAGS=""

OS=${1:-linux}
ARCH=${2:-amd64}

# Fallback to 10.15 only if MACOSX_DEPLOYMENT_TARGET was not passed into the container
export MACOSX_DEPLOYMENT_TARGET="${MACOSX_DEPLOYMENT_TARGET:-10.15}"

# If targeting Apple Silicon, strictly enforce a minimum of macOS 11.0
if [[ "$OS" == "darwin" && "$ARCH" == "arm64" ]]; then
    # Extract the major version (e.g., "10" from "10.15", "11" from "11.3")
    MAC_MAJOR="${MACOSX_DEPLOYMENT_TARGET%%.*}"
    
    if [[ "$MAC_MAJOR" -lt 11 ]]; then
        echo ">>> [Notice] Target is darwin-arm64 but incoming MACOSX_DEPLOYMENT_TARGET is '${MACOSX_DEPLOYMENT_TARGET}'."
        echo ">>> Upgrading target to 11.0 to prevent compiler/linker version mismatches."
        export MACOSX_DEPLOYMENT_TARGET="11.0"
    fi
fi

# -----------------------------------------------------------------------------
# Cross-compilation environment setup
# -----------------------------------------------------------------------------
RANLIB_CMD="ranlib"
LD_PATH=""
PLATFORM_LIBS=""
TRIPLE=""
STRIP=""
AR=""
CFLAGS=""
CXXFLAGS=""
LDFLAGS=""
MESON_ARCH=""
NM_CMD="nm"
CC=""

case "${OS}-${ARCH}" in
    windows-amd64 | windows-arm64)
        if [ "$ARCH" = "amd64" ]; then
            MESON_ARCH="x86_64"; TRIPLE="x86_64-w64-mingw32"
        else
            MESON_ARCH="aarch64"; TRIPLE="aarch64-w64-mingw32"
        fi
        MESON_SYSTEM="windows"
        CC="$LLVM_MINGW_PATH/bin/${TRIPLE}-clang"
        CXX="$LLVM_MINGW_PATH/bin/${TRIPLE}-clang++"
        AR="$LLVM_MINGW_PATH/bin/${TRIPLE}-ar"
        NM_CMD="$LLVM_MINGW_PATH/bin/${TRIPLE}-nm"
        STRIP="$LLVM_MINGW_PATH/bin/${TRIPLE}-strip"
        WINDRES="$LLVM_MINGW_PATH/bin/${TRIPLE}-windres"
        RANLIB_CMD="$LLVM_MINGW_PATH/bin/${TRIPLE}-ranlib"
        LD_PATH="$LLVM_MINGW_PATH/bin/${TRIPLE}-ld"
        CFLAGS="-Wno-unused-command-line-argument"
        CXXFLAGS="-Wno-unused-command-line-argument"
        LDFLAGS="-Wno-unused-command-line-argument"
        PLATFORM_LIBS="-lws2_32 -lstdc++"
        PROPERTIES="
needs_exe_wrapper = true
has_function_printf = true
has_function_hf_printf = false
growing_stack = false
"
        ;;
    linux-amd64 | linux-arm64)
        if [ "$ARCH" = "amd64" ]; then
            MESON_ARCH="x86_64"; TGT="x86_64-linux-gnu"
        else
            MESON_ARCH="aarch64"; TGT="aarch64-linux-gnu"
            # ARM64 assembly links perfectly fine with lld
            # EXTRA_FFMPEG_FLAGS="--disable-asm"
        fi
        MESON_SYSTEM="linux"
        TRIPLE="$TGT"
        CC="/usr/bin/clang"
        CXX="/usr/bin/clang++"
        AR="/usr/bin/llvm-ar"
        NM_CMD="nm"
        STRIP="/usr/bin/llvm-strip"
        RANLIB_CMD="/usr/bin/llvm-ranlib"
        LD_PATH="/usr/bin/ld.lld"
        CFLAGS="-target $TGT -fPIC"
        CXXFLAGS="-target $TGT -fPIC"
        LDFLAGS="-target $TGT -fuse-ld=lld -lm"
        PLATFORM_LIBS="-lstdc++ -lm"
        PROPERTIES=""
        ;;
    darwin-amd64 | darwin-arm64)
        if [ "$ARCH" = "amd64" ]; then
            MESON_ARCH="x86_64"; TGT="x86_64-apple-macos$MACOSX_DEPLOYMENT_TARGET"
            # Bypass NASM assembly to prevent LLVM lld linker segmentation faults
            EXTRA_FFMPEG_FLAGS="--disable-x86asm"
        else
            MESON_ARCH="aarch64"; TGT="arm64-apple-macos$MACOSX_DEPLOYMENT_TARGET"
            # ARM64 assembly links perfectly fine with lld
            EXTRA_FFMPEG_FLAGS="--disable-asm"
        fi
        MESON_SYSTEM="darwin"
        TRIPLE="${MESON_ARCH}-apple-darwin"
        LDFLAGS=""
        FFMPEG_OS_FLAGS="--enable-videotoolbox --enable-audiotoolbox ${EXTRA_FFMPEG_FLAGS}"

        # Darwin Linker Wrapper
        mkdir -p /tmp/darwin-tools
        cat << 'EOF' > /tmp/darwin-tools/ld
#!/bin/bash
# FALLBACK SAFEGUARD: If the host Linux compiler accidentally triggers this 
# because it's first in PATH, immediately hand off to the real Linux linker.
for arg in "$@"; do
    if [[ "$arg" == *plugin* || "$arg" == *-maarch64linux* || "$arg" == *-melf_* || "$arg" == *elf64* ]]; then
        exec /usr/bin/ld "$@"
    fi
done

# Re-filter and translate arguments for Darwin LLD (ld64 flavor)

# STEP 1: Flatten and Sanitize Arguments
# Libtool passes packed flags like -Wl,-platform_version,macos,target_version,target_version
# We flatten these into raw, individual arguments so the 'case' loop can see them.
raw_args=()
for item in "$@"; do
    if [[ "$item" == -Wl,* ]]; then
        IFS=',' read -ra ADDR <<< "${item#-Wl,}"
        raw_args+=("${ADDR[@]}")
    elif [[ "$item" == -Xlinker,* ]]; then
        IFS=',' read -ra ADDR <<< "${item#-Xlinker,}"
        raw_args+=("${ADDR[@]}")
    else
        raw_args+=("$item")
    fi
done
set -- "${raw_args[@]}"

# STEP 2: Process Arguments
args=()
# CRITICAL: Initialize ARCH_FOUND to false so the failsafe knows if it needs to act
ARCH_FOUND=false
# CRITICAL: Initialize platform version tracking
PLATFORM_VERSION_FOUND=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        # Translate ELF whole-archive to Darwin all_load
        --whole-archive|-whole-archive)
            args+=("-all_load")
            shift
            ;;
        --no-whole-archive|-no-whole-archive)
            shift 
            ;;
        # Translate ELF soname to Darwin install_name
        -soname)
            # FIX: Converts "libvips.dylib.42" to "libvips.42.dylib"
            # The regex captures the version number and swaps the extension order
            REAL_NAME=$(echo "$2" | sed -E 's/\.dylib\.([0-9]+)$/.\1.dylib/')
            args+=("-install_name" "@rpath/$REAL_NAME")
            shift 2
            ;;
        # Map Linux -shared to Darwin -dylib (fixes Libtool injecting -shared)
        -shared)
            args+=("-dylib")
            shift
            ;;
        # Drop ELF groups (Darwin doesn't need them)
        --start-group|--end-group)
            shift
            ;;
        # Drop version scripts (ELF concept, unsupported by Mach-O)
        --version-script)
            # Drop the flag and its following file argument
            shift 2
            ;;
        --version-script=*|--dynamic-linker=*)
            # Drop inline formats
            shift
            ;;
        # Drop incompatible ELF and Libtool-injected noise (e.g. from libffi)
        --as-needed|--no-as-needed|--no-undefined|--allow-shlib-undefined|--build-id|--eh-frame-hdr|-EL)
            shift
            ;;
        # Drop Linux-specific dynamic linker flags
        --dynamic-linker|-dynamic-linker)
            # Catches: --dynamic-linker /lib/ld...
            shift 2
            ;;
        --hash-style=*)
            shift
            ;;
        --sysroot)
            # Drop Linux-style sysroot; Darwin uses -syslibroot handled via compiler drivers
            shift 2
            ;;
        # Drop Linux System Objects and Libraries
        # Libtool tries to use Linux start files/standard libs for Darwin
       *crti.o|*crtn.o|*crtbeginS.o|*crtendS.o|*crtbegin.o|*crtend.o|*Scrt1.o)
            shift
            ;;
        -lgcc_s|-lgcc|-lc|-lm)
            # Darwin uses libSystem; Linux-style -lgcc_s / -lc will fail
            shift
            ;;
        -m) # Drop obsolete emulation flag
            shift 2
            ;;
        # CRITICAL: Detect if Clang/Libtool actually passed the arch
        -arch)
            ARCH_FOUND=true
            args+=("$1" "$2")
            shift 2
            ;;
        # Track Darwin platform version flags
        -platform_version)
            PLATFORM_VERSION_FOUND=true
            # This flag takes 3 arguments after it (e.g., macos target_version target_version)
            args+=("$1" "$2" "$3" "$4")
            shift 4
            ;;
        -macosx_version_min)
            # Alternate older flag sometimes passed by Clang
            PLATFORM_VERSION_FOUND=true
            # Map older min-version to modern platform-version for consistency
            args+=("-platform_version" "macos" "$2" "$2")
            shift 2
            ;;
        # Swallow flags that LLD Darwin doesn't support yet to prevent warnings/errors
        # -r|-keep_private_externs)
        #     shift
        #     ;;
        *)
            args+=("$1")
            shift
            ;;
    esac
done

# Failsafe: If Libtool/Clang didn't pass -arch, explicitly force the target architecture
# This prevents the "lld: error: must specify -arch" failure.
if [ "$ARCH_FOUND" = false ]; then
    TARGET_ARCH="__TARGET_ARCH__"
    if [ "$TARGET_ARCH" = "aarch64" ]; then TARGET_ARCH="arm64"; fi
    args=("-arch" "$TARGET_ARCH" "${args[@]}")
fi

# Failsafe: Inject missing -platform_version for Mach-O requirements
if [ "$PLATFORM_VERSION_FOUND" = false ]; then
    args=("-platform_version" "macos" "__DEPLOYMENT_TARGET__" "__DEPLOYMENT_TARGET__" "${args[@]}")
fi

# Execute with explicit Darwin flavor
exec /usr/bin/lld -flavor darwin "${args[@]}"
EOF
        chmod +x /tmp/darwin-tools/ld

        # INJECT TEMPLATE VARIABLES USING SED
        sed -i "s/__TARGET_ARCH__/${MESON_ARCH}/g" /tmp/darwin-tools/ld
        sed -i "s/__DEPLOYMENT_TARGET__/${MACOSX_DEPLOYMENT_TARGET:-10.15}/g" /tmp/darwin-tools/ld

        # Reference your wrapper created earlier in the block
        LD_PATH="/tmp/darwin-tools/ld"

        # Symlink all possible target queries so they resolve back to our wrapper
        ln -sf /tmp/darwin-tools/ld /tmp/darwin-tools/ld64.lld
        ln -sf /tmp/darwin-tools/ld /tmp/darwin-tools/ld64
        ln -sf /tmp/darwin-tools/ld /tmp/darwin-tools/x86_64-apple-darwin-ld
        ln -sf /tmp/darwin-tools/ld /tmp/darwin-tools/x86_64-apple-darwin-ld64
        ln -sf /tmp/darwin-tools/ld /tmp/darwin-tools/x86_64-apple-darwin11.0-ld
        ln -sf /tmp/darwin-tools/ld /tmp/darwin-tools/x86_64-apple-darwin11.0-ld64
        ln -sf /tmp/darwin-tools/ld /tmp/darwin-tools/x86_64-apple-darwin11.0-ld64.lld
        ln -sf /tmp/darwin-tools/ld /tmp/darwin-tools/arm64-apple-darwin-ld
        ln -sf /tmp/darwin-tools/ld /tmp/darwin-tools/arm64-apple-darwin-ld64
        ln -sf /tmp/darwin-tools/ld /tmp/darwin-tools/arm64-apple-darwin11.0-ld
        ln -sf /tmp/darwin-tools/ld /tmp/darwin-tools/arm64-apple-darwin11.0-ld64
        ln -sf /tmp/darwin-tools/ld /tmp/darwin-tools/arm64-apple-darwin11.0-ld64.lld

        # Use absolute system paths to avoid llvm-mingw interference
        # and ensure Autotools/Libtool find the correct indexer
        CC="/usr/bin/clang"
        CXX="/usr/bin/clang++"
        OBJC="/usr/bin/clang"
        AR="/usr/bin/llvm-ar"
        STRIP="/usr/bin/llvm-strip"
        RANLIB_CMD="/usr/bin/llvm-ranlib"
        NM_CMD="/usr/bin/llvm-nm"

        # Use Proxy Flags (-B) to force clang to use our wrapper
        PROXY_FLAGS="-B/tmp/darwin-tools"
        SYSROOT_FLAGS="--sysroot=$SDK_PATH $PROXY_FLAGS -target $TGT"

        # Define SDKROOT for system header discovery (fixes res_query detection)
        export SDKROOT="$SDK_PATH"

        # Define Framework Path
        FRAMEWORK_PATH="$SDK_PATH/System/Library/Frameworks"
        
        # Add -fno-asynchronous-unwind-tables to CFLAGS
        # This fixes the 'invalid CFI advance_loc expression' in libffi assembly
        # Add -fPIC: explicitly ensure PIC for Darwin static archives to be linked into dylib.
        CFLAGS="$SYSROOT_FLAGS -DTARGET_OS_OSX=1 -F$FRAMEWORK_PATH -fno-asynchronous-unwind-tables -fPIC -Wno-unused-command-line-argument -Wno-error=partial-availability -Wno-partial-availability"
        # FORCE libc++ to prevent clang++ from seeking libstdc++ on the host
        CXXFLAGS="$CFLAGS -stdlib=libc++"

        # Locate the compiler-rt static archive built earlier
        # Use absolute path to bypass the Mingw toolchain in our PATH
        RESOURCE_DIR=$(/usr/bin/clang -print-resource-dir)
        COMPILER_RT_ARCHIVE="${RESOURCE_DIR}/lib/darwin/libclang_rt.builtins_osx.a"

        # Add -lresolv specifically for res_query() and isolated Darwin libs
        # Modern macOS uses libc++ NOT lstdc++
        # Inject the natively built compiler-rt archive directly into the linker path
        PLATFORM_LIBS="-lSystem -liconv -lresolv -lc++ ${COMPILER_RT_ARCHIVE} -framework CoreFoundation -framework CoreMedia -framework CoreVideo -framework VideoToolbox -framework AudioToolbox -framework CoreGraphics -framework Security"
        # Propagate target flags to the linker so CMake knows we are cross-compiling
        # Include -stdlib=libc++ in LDFLAGS so feature checks don't fail looking for libstdc++
        export LDFLAGS="$CFLAGS -stdlib=libc++ $PLATFORM_LIBS --ld-path=$LD_PATH -L$SDK_PATH/usr/lib -F$FRAMEWORK_PATH -Wl,-platform_version,macos,$MACOSX_DEPLOYMENT_TARGET,$MACOSX_DEPLOYMENT_TARGET"
        
        PROPERTIES="iconv = 'libc'"
        ;;
    *) echo "Unsupported target: ${OS}-${ARCH}"; exit 1 ;;
esac

echo "DEBUG: Building FFmpeg for ${OS}-${ARCH}"
echo "DEBUG: CFLAGS  = $CFLAGS"
echo "DEBUG: LDFLAGS = $LDFLAGS"

WORKDIR="/build"
mkdir -p $WORKDIR
cd $WORKDIR
export WORKDIR

# Generate cross files
CROSS_FILE="cross_${OS}_${ARCH}.txt"
sysroot="/sysroot/${OS}_${ARCH}"
if [ -d "$sysroot" ]; then rm -rf "${sysroot}"; fi
mkdir -p "${sysroot}"/{usr/include,usr/lib/pkgconfig,include,lib/pkgconfig,lib,bin,share}

format_meson_array() {
    if [ -z "$1" ]; then return; fi
    echo "$1" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//;s/[[:space:]][[:space:]]*/ /g' | sed "s/ /', '/g" | sed "s/^/'/" | sed "s/$/'/"
}

LD_BIN_OVERRIDE=""
if [ -n "$LD_PATH" ]; then
    LD_BIN_OVERRIDE="c_ld = '$LD_PATH'
cpp_ld = '$LD_PATH'"
fi

BUILTIN_OPTS=""
if [[ "$MESON_SYSTEM" == "darwin" ]]; then
    BUILTIN_OPTS="
b_asneeded = false
b_lundef = false"
fi

EXTRA_MESON_INCLUDES=""
if [[ "$MESON_SYSTEM" == "darwin" ]]; then
    EXTRA_MESON_INCLUDES=", '-I$SDK_PATH/usr/include'"
fi

cat <<EOF > "${CROSS_FILE}"
[binaries]
c = ['$CC']
cpp = ['$CXX']
objc = ['$CC']
ar = ['$AR']
strip = ['$STRIP']
pkg-config = 'pkg-config'
windres = ['$WINDRES']
iconv = '${sysroot}/bin/iconv'
$LD_BIN_OVERRIDE

[host_machine]
system = '${MESON_SYSTEM}'
cpu_family = '${MESON_ARCH}'
cpu = '${MESON_ARCH}'
endian = 'little'

[properties]
pkg_config_path = '${sysroot}/lib/pkgconfig:${sysroot}/lib64/pkgconfig:${sysroot}/usr/lib/pkgconfig:${sysroot}/share/pkgconfig:/usr/share/pkgconfig'
pkg_config_libdir = ''
pkg_config_sysroot_dir = '${sysroot}'
iconv_link_args = ['-liconv']
${PROPERTIES}

[built-in options]
${BUILTIN_OPTS}
# Add explicit include paths so the compiler finds headers for your custom deps
# Use format_meson_array to ensure Meson parses individual flags correctly
c_args = [$(format_meson_array "$CFLAGS"), '-I${sysroot}/include'${EXTRA_MESON_INCLUDES}, '-Wno-error=missing-include-dirs', '-DLIBICONV_PLUG']
cpp_args = [$(format_meson_array "$CXXFLAGS"), '-I${sysroot}/include'${EXTRA_MESON_INCLUDES}, '-Wno-error=missing-include-dirs']
objc_args = [$(format_meson_array "$CFLAGS"), '-I${sysroot}/include'${EXTRA_MESON_INCLUDES}, '-Wno-error=missing-include-dirs']

# Add library paths and the missing de265/C++ flags for the final link
c_link_args = [$(format_meson_array "$LDFLAGS"), '-lde265', $(format_meson_array "$PLATFORM_LIBS"), '-L${sysroot}/lib', '-L${sysroot}/usr/lib']
cpp_link_args = [$(format_meson_array "$LDFLAGS"), '-lde265', $(format_meson_array "$PLATFORM_LIBS"), '-L${sysroot}/lib', '-L${sysroot}/usr/lib']
objc_link_args = [$(format_meson_array "$LDFLAGS"), '-lde265', $(format_meson_array "$PLATFORM_LIBS"), '-L${sysroot}/lib', '-L${sysroot}/usr/lib']

default_library = 'static'
EOF

# Generate CMake toolchain file
CMAKE_TOOLCHAIN="toolchain_${OS}_${ARCH}.cmake"
echo "Generating ${CMAKE_TOOLCHAIN} in ${WORKDIR}..."

capitalized_system="${MESON_SYSTEM^}"   # windows -> Windows, darwin -> Darwin, linux -> Linux

cat > "${CMAKE_TOOLCHAIN}" <<EOF
set(CMAKE_SYSTEM_NAME ${capitalized_system})
set(CMAKE_SYSTEM_PROCESSOR ${MESON_ARCH})
set(CMAKE_C_COMPILER "${CC}")
set(CMAKE_CXX_COMPILER "${CXX}")
set(CMAKE_ASM_COMPILER "${CC}")
set(CMAKE_AR "${AR}")
set(CMAKE_STRIP "${STRIP}")
set(CMAKE_RC_COMPILER "${WINDRES}")
set(CMAKE_EXE_LINKER_FLAGS "${LDFLAGS}")
set(CMAKE_MAKE_PROGRAM "/usr/bin/ninja" CACHE FILEPATH "Ninja build tool")

# INJECT FLAGS DIRECTLY INTO CMAKE VARIABLES
# This ensures compilation steps (not just linking) use the correct target.
set(CMAKE_C_FLAGS_INIT "${CFLAGS}")
set(CMAKE_CXX_FLAGS_INIT "${CXXFLAGS}")
set(CMAKE_ASM_FLAGS_INIT "${CFLAGS}")
set(CMAKE_EXE_LINKER_FLAGS_INIT "${LDFLAGS}")
set(CMAKE_SHARED_LINKER_FLAGS_INIT "${LDFLAGS}")
set(CMAKE_MODULE_LINKER_FLAGS_INIT "${LDFLAGS}")

set(CMAKE_FIND_ROOT_PATH "${sysroot}")
set(CMAKE_FIND_ROOT_PATH_MODE_PROGRAM NEVER)
set(CMAKE_FIND_ROOT_PATH_MODE_LIBRARY ONLY)
set(CMAKE_FIND_ROOT_PATH_MODE_INCLUDE ONLY)

set(CMAKE_INSTALL_PREFIX "${sysroot}")

# For macOS
if("${MESON_SYSTEM}" STREQUAL "darwin")
  set(CMAKE_OSX_SYSROOT "${SDK_PATH}")
  set(CMAKE_INSTALL_NAME_TOOL "/usr/bin/llvm-install-name-tool")
  set(CMAKE_OTOOL "/usr/bin/llvm-otool")
  # FORCE SHRED LINKER TO PASS GLOBAL LDFLAGS ON DARWIN
  set(CMAKE_SHARED_LINKER_FLAGS "${LDFLAGS}" CACHE STRING "" FORCE)
endif()
EOF

# -----------------------------------------------------------------------------
# Build fingerprint / checksum
# -----------------------------------------------------------------------------
ENABLED_FEATURES="gpl,version3,static,pic,libx264,libx265,libvpx,libaom,libdav1d,libopus,libvorbis,libmp3lame,libwebp,libpng,libjpeg,libtiff,libass,zlib,bzlib,lzma,subtitles,image-export"
BUILD_FINGERPRINT="${FFMPEG_VERSION}|${OS}|${ARCH}|${ZLIB_VERSION}|${LIBPNG_VERSION}|${LIBJPEG_TURBO_VERSION}|${LIBWEBP_VERSION}|${TIFF_VERSION}|${FREETYPE_VERSION}|${HARFBUZZ_VERSION}|${BROTLI_VERSION}|${LIBDE265_VERSION}|${FRIBIDI_VERSION}|${EXPAT_VERSION}|${FONTCONFIG_VERSION}|${LIBASS_VERSION}|${OPUS_VERSION}|${LIBVPX_VERSION}|${X264_VERSION}|${X265_VERSION}|${DAV1D_VERSION}|${LAME_VERSION}|${ENABLED_FEATURES}|${MACOSX_DEPLOYMENT_TARGET:-10.15}|$(cat ${CROSS_FILE})|${LIBAOM_VERSION}|${LIBVORBIS_VERSION}|${LIBOGG_VERSION}|${BZIP2_VERSION}|${XZ_VERSION}|${FFMPEG_OS_FLAGS}|${MINIAUDIO_VERSION}"
CURRENT_CHECKSUM=$(echo "$BUILD_FINGERPRINT" | sha256sum | awk '{print $1}')

if [ -f "/output/build.checksum" ]; then
    STORED_CHECKSUM=$(cat "/output/build.checksum")
    if [ "$CURRENT_CHECKSUM" = "$STORED_CHECKSUM" ]; then
        echo "========================================================="
        echo " CHECKSUM MATCH: No changes for ${OS}-${ARCH}."
        echo " Fingerprint: ${CURRENT_CHECKSUM}"
        echo " Skipping repetitive build."
        echo "========================================================="
        exit 0
    fi
fi

if [ "${MESON_SYSTEM}" = "darwin" ]; then
    echo "set(CMAKE_OSX_SYSROOT \"${SDK_PATH}\")" >> ${CMAKE_TOOLCHAIN}
fi
if [ -n "${WINDRES}" ]; then
    echo "set(CMAKE_RC_COMPILER ${WINDRES})" >> ${CMAKE_TOOLCHAIN}
fi

# -----------------------------------------------------------------------------
# Helper: build_dep
# -----------------------------------------------------------------------------
build_dep() {
    local name="$1"
    local url="$2"
    local version="$3"
    local build_type="$4"
    local extra_flags="${5:-}"
    local ext="${6:-.tar.gz}"
    local extract_cmd="${7:-tar -xzf}"

    echo ">>> Building ${name} (${version}) for ${OS}-${ARCH}..."

    # Handle packages where the downloaded filename uses underscore instead of hyphen
    local tarball_name="${name}-${version}"
    if [[ "$name" == "x265" ]]; then
        tarball_name="${name}_${version}"
    fi

    local tarball="${tarball_name}${ext}"
    local src_dir="${WORKDIR}/${name}-${version}"

    cd "${WORKDIR}"
    if [ ! -f "${tarball}" ]; then
        curl -L -f "${url}" -o "${tarball}" || { echo "Failed to download ${url}"; exit 1; }
    fi

    if [ -d "${src_dir}" ]; then rm -rf "${src_dir}"; fi
    ${extract_cmd} "${tarball}"

    # Fix extracted directory name for x265 (x265_3.6 -> x265-3.6)
    if [[ "$name" == "x265" ]]; then
        local extracted_dir="${WORKDIR}/${name}_${version}"
        if [ -d "${extracted_dir}" ] && [ ! -d "${src_dir}" ]; then
            mv "${extracted_dir}" "${src_dir}"
        fi
    fi

    case "${build_type}" in
        header-only)
            echo ">>> Copying header assets for ${name} to ${sysroot}/include..."
            mkdir -p "${sysroot}/include"
            
            # Use extra_flags to pass the specific header file name(s) we want to copy
            local target_header="${extra_flags}"
            
            if [ -f "${src_dir}/${target_header}" ]; then
                cp "${src_dir}/${target_header}" "${sysroot}/include/"
                echo ">>> Successfully copied ${target_header} to ${sysroot}/include/"
            else
                # Fallback search if directory structure differs
                local found_file=$(find "${src_dir}" -name "${target_header}" -print -quit)
                if [ -n "${found_file}" ]; then
                    cp "${found_file}" "${sysroot}/include/"
                    echo ">>> Successfully found and copied ${target_header} to ${sysroot}/include/"
                else
                    echo "ERROR: Could not find ${target_header} in ${src_dir}"
                    exit 1
                fi
            fi
            ;;
        meson)
            BUILD_DIR="${src_dir}/build_${OS}_${ARCH}"
            find "${src_dir}" -exec touch -t $(date +%Y%m%d%H%M.%S) {} + 2>/dev/null || true
            meson setup "${BUILD_DIR}" "${src_dir}" \
                --cross-file "${WORKDIR}/${CROSS_FILE}" \
                --prefix="${sysroot}" \
                ${MESON_CONF_FLAGS} \
                --buildtype=release \
                -Db_staticpic=true \
                ${extra_flags}
            meson compile -C "${BUILD_DIR}" -j $(nproc) -l $(nproc)
            meson install -C "${BUILD_DIR}"
            ;;
        cmake)
            # x265 has CMakeLists.txt inside a "source/" subdirectory
            local cmake_source_dir="${src_dir}"
            local CMAKE_EXTRA_ARGS=""

            if [[ "$name" == "x265" ]]; then
                cmake_source_dir="${src_dir}/source"
                
                # FIX: Disable assembly for macOS arm64 to avoid host-GNU assembler 
                # fallback and symbol linking failures.
                if [[ "$OS" == "darwin" && "$ARCH" == "arm64" ]]; then
                    echo ">>> [x265 Special Case] macOS arm64 target detected. Disabling incompatible assembly compilation."
                    CMAKE_EXTRA_ARGS="-DENABLE_ASSEMBLY=OFF"
                fi
            fi

            TOOLCHAIN_ABS="${WORKDIR}/${CMAKE_TOOLCHAIN}"
            BUILD_DIR="${src_dir}/build_${OS}_${ARCH}"

            cmake -S "${cmake_source_dir}" -B "${BUILD_DIR}" \
                -G Ninja \
                -DCMAKE_TOOLCHAIN_FILE="${TOOLCHAIN_ABS}" \
                ${CMAKE_CONF_FLAGS} \
                -DCMAKE_BUILD_TYPE=Release \
                -DCMAKE_POSITION_INDEPENDENT_CODE=ON \
                -DCMAKE_INSTALL_PREFIX="${sysroot}" \
                -DCMAKE_INSTALL_LIBDIR=lib \
                -DPKGCONFIG_INSTALL_DIR=lib/pkgconfig \
                ${CMAKE_EXTRA_ARGS} \
                ${extra_flags}
            cmake --build "${BUILD_DIR}" --parallel $(nproc)
            cmake --install "${BUILD_DIR}"

            # Manually create zlib.pc and fix library name for zlib
            if [ "$name" = "zlib" ]; then
              echo "DEBUG: Fixing compiled zlib static assets..."

              # FIX: Copy libzs.a to libz.a so autotools (libpng) can find it with -lz
              for file in libzs.a libzlibstatic.a libzlib.a libz_static.a zlib.a; do
                  if [ -f "${sysroot}/lib/${file}" ]; then
                      echo "DEBUG: Compat - Copying ${file} to libz.a"
                      cp "${sysroot}/lib/${file}" "${sysroot}/lib/libz.a"
                      break
                  fi
              done

              mkdir -p "${sysroot}/lib/pkgconfig"
              cat > "${sysroot}/lib/pkgconfig/zlib.pc" <<EOF
prefix=${sysroot}
exec_prefix=\${prefix}
libdir=\${exec_prefix}/lib
includedir=\${prefix}/include

Name: zlib
Description: zlib compression library
Version: ${ZLIB_VERSION}

Libs: -L\${libdir} -lz
Cflags: -I\${includedir}
EOF
                echo "DEBUG: Manually created zlib.pc in /lib/pkgconfig"
                cat "${sysroot}/lib/pkgconfig/zlib.pc"
            fi
            ;;
        autotools)
          # Define extra flags specifically for C and C++ builds
          EXTRA_AUTOTOOLS_CFLAGS=""
          EXTRA_AUTOTOOLS_CXXFLAGS=""
          EXTRA_CONF_ARGS="--with-gnu-ld"
          
          if [[ "$OS" == "windows" ]]; then
            # PCRE2 requires this for static linking to avoid __imp_ prefix mismatches
            if [[ "$DEP_LIBRARY_TYPE" == "static" ]]; then
                EXTRA_AUTOTOOLS_CFLAGS="-DPCRE2_STATIC"
                EXTRA_AUTOTOOLS_CXXFLAGS="-DPCRE2_STATIC"
            fi
          fi

          # Cross-platform Libtool Sanitation
          local CONF_ENV=""
          # Default to the standard LD if available, otherwise let the compiler decide
          local ACTUAL_LD="${LD:-ld}"

          if [[ "$OS" == "darwin" ]]; then
            ACTUAL_LD="/tmp/darwin-tools/ld"
            # FORCE LIBTOOL TO AVOID RELOCATABLE LINKS (-r)
            # 1. lt_cv_apple_embedded: Force Apple-style dynamic linking conventions
            # 2. lt_cv_deplibs_check_method: Prevent host-library checks that fail in cross-mode
            # 3. with_gnu_ld: Force Libtool to bypass legacy Darwin -r hacks by treating lld as GNU-compatible
            CONF_ENV="lt_cv_apple_embedded=yes lt_cv_deplibs_check_method=pass_all with_gnu_ld=yes"
            # EXTRA_CONF_ARGS="--with-gnu-ld"
            
            # Force Clang to use Apple's libc++ instead of the Linux host's GNU libstdc++
            EXTRA_AUTOTOOLS_CXXFLAGS="-stdlib=libc++"

            # Create target-prefixed symlinks so Autotools naturally discovers our wrapper
            mkdir -p /tmp/darwin-tools
            ln -sf /tmp/darwin-tools/ld /tmp/darwin-tools/${TRIPLE}-ld
            export PATH="/tmp/darwin-tools:$PATH"
          elif [[ "$OS" == "linux" ]]; then
            # Ensure Libtool finds the cross-compiled libraries in our sysroot
            ACTUAL_LD="${TRIPLE}-ld"
            CONF_ENV="lt_cv_sys_lib_dlsearch_path_spec=${sysroot}/lib"
          fi
          
          # Extract target flag to ensure Clang knows the OS
          local TARGET_FLAG=$(echo "$CFLAGS" | grep -oE -- '-target [^ ]+' || true)
          
          # Bind the target AND the linker paths directly to the CC/CXX variables.
          # By making -B and --ld-path part of the core command string, we make it 
          # impossible for Libtool's CCLD phase to strip them out. This permanently
          # isolates the build from the host's /usr/bin/ld.
          local LOCAL_CC="${CC} ${TARGET_FLAG}"
          local LOCAL_CXX="${CXX} ${TARGET_FLAG}"

          if [[ "$OS" == "darwin" ]]; then
            # Only inject the Darwin wrapper when targeting macOS
            LOCAL_CC="${LOCAL_CC} -B/tmp/darwin-tools --ld-path=/tmp/darwin-tools/ld"
            LOCAL_CXX="${LOCAL_CXX} -B/tmp/darwin-tools --ld-path=/tmp/darwin-tools/ld"
          fi

          # =====================================================
          # SPECIAL CASE FOR LIBVPX (must stay inside autotools)
          # =====================================================
          if [[ "$name" == "libvpx" ]]; then
            echo ">>> Building ${name} (${version}) for ${OS}-${ARCH} (libvpx special path)..."

            # Prevent Clang from treating macOS target version mismatches as fatal errors
            if [[ "$OS" == "darwin" ]]; then
                EXTRA_AUTOTOOLS_CFLAGS="${EXTRA_AUTOTOOLS_CFLAGS} -Wno-error=overriding-option -Wno-overriding-option"
                EXTRA_AUTOTOOLS_CXXFLAGS="${EXTRA_AUTOTOOLS_CXXFLAGS} -Wno-error=overriding-option -Wno-overriding-option"
            fi

            # Calculate correct Darwin Kernel version from macOS deployment target
            if [[ "$OS" == "darwin" ]]; then
                MAC_VER="${MACOSX_DEPLOYMENT_TARGET:-10.15}"
                MAC_MAJOR="${MAC_VER%%.*}"
                
                if [ "$MAC_MAJOR" = "10" ]; then
                    MAC_MINOR=$(echo "$MAC_VER" | cut -d. -f2)
                    DARWIN_VER=$((MAC_MINOR + 4))
                else
                    DARWIN_VER=$((MAC_MAJOR + 9))
                fi

                # SAFEGUARD: Apple Silicon (arm64) does not exist before Darwin 20 (macOS 11).
                # Force Darwin 20+ for arm64 so libvpx recognizes the toolchain target.
                if [[ "$ARCH" == "arm64" && $DARWIN_VER -lt 20 ]]; then
                    DARWIN_VER=20
                fi
            fi

            # Resolve correct target for libvpx
            case "${OS}-${ARCH}" in
                linux-amd64)  LIBVPX_TARGET="x86_64-linux-gcc" ;;
                linux-arm64)  LIBVPX_TARGET="arm64-linux-gcc" ;;
                darwin-amd64)  LIBVPX_TARGET="x86_64-darwin${DARWIN_VER}-gcc" ;;
                darwin-arm64)  LIBVPX_TARGET="arm64-darwin${DARWIN_VER}-gcc" ;;
                windows-amd64) LIBVPX_TARGET="x86_64-win64-gcc" ;;
                windows-arm64) LIBVPX_TARGET="arm64-win64-gcc" ;;
                *)             LIBVPX_TARGET="generic-gnu" ;;
            esac

            # Strip global shared/static flags passed into the script
            VPX_CONF_FLAGS=$(echo "${extra_flags}" | sed 's/--disable-static//g' | sed 's/--enable-shared//g')

            # Enforce static compilation globally
            VPX_CONF_FLAGS="${VPX_CONF_FLAGS} --enable-static --disable-shared"

            # Run configure with the sanitized flags
            (cd "${src_dir}" && \
                env ${CONF_ENV} \
                    LD="${ACTUAL_LD}" \
                    CC="${LOCAL_CC}" \
                    CXX="${LOCAL_CXX}" \
                    AR="${AR}" \
                    STRIP="${STRIP}" \
                    RANLIB="${RANLIB_CMD:-ranlib}" \
                    CFLAGS="${CFLAGS} -fPIC -O3 ${EXTRA_AUTOTOOLS_CFLAGS}" \
                    CXXFLAGS="${CXXFLAGS} -fPIC -O3 ${EXTRA_AUTOTOOLS_CXXFLAGS}" \
                    CPPFLAGS="-I${sysroot}/include" \
                    LDFLAGS="-L${sysroot}/lib ${LDFLAGS} ${PLATFORM_LIBS}" \
                    PKG_CONFIG_PATH="${sysroot}/lib/pkgconfig:${sysroot}/usr/lib/pkgconfig:${sysroot}/share/pkgconfig" \
                ./configure \
                    --target=${LIBVPX_TARGET} \
                    --prefix="${sysroot}" \
                    --extra-cflags="${CFLAGS} -fPIC -O3 ${EXTRA_AUTOTOOLS_CFLAGS}" \
                    --extra-cxxflags="${CXXFLAGS} -fPIC -O3 ${EXTRA_AUTOTOOLS_CXXFLAGS}" \
                    --enable-vp8 \
                    --enable-vp9 \
                    --enable-multithread \
                    --enable-runtime-cpu-detect \
                    --disable-examples \
                    --disable-tools \
                    --disable-docs \
                    --disable-unit-tests \
                    ${VPX_CONF_FLAGS})

          elif [[ "$name" == "x264" ]]; then
            echo ">>> Building ${name} (${version}) for ${OS}-${ARCH} (${name} special path)..."

            # x264 doesn't accept standard autotools library parameters; clean them out
            X264_CONF_FLAGS=$(echo "${extra_flags}" | sed 's/--disable-static//g' | sed 's/--enable-shared//g')

            # Default configuration for x264 across all standard platforms
            local EXTRA_LDFLAGS=""
            local EXTRA_CONFIG=""

            if [[ "$OS" == "darwin" ]]; then
                EXTRA_LDFLAGS="-Wl,-read_only_relocs,suppress"
                # DISABLING ASM: The handwritten assembly in x264 (both x86_64 and AArch64) 
                # generates relocations that cause LLVM's lld-macho cross-linker to segfault. 
                # Disabling ASM forces C-fallback paths, allowing successful linking.
                EXTRA_CONFIG="--disable-asm"
            fi

            (cd "${src_dir}" && \
                env ${CONF_ENV} \
                    LD="${ACTUAL_LD}" \
                    CC="${LOCAL_CC}" \
                    CXX="${LOCAL_CXX}" \
                    AR="${AR}" \
                    STRIP="${STRIP}" \
                    STRINGS="/usr/bin/llvm-strings" \
                    RANLIB="${RANLIB_CMD:-ranlib}" \
                    CFLAGS="${CFLAGS} -fPIC -O3 ${EXTRA_AUTOTOOLS_CFLAGS}" \
                    CXXFLAGS="${CXXFLAGS} -fPIC -O3 ${EXTRA_AUTOTOOLS_CXXFLAGS}" \
                    CPPFLAGS="-I${sysroot}/include" \
                    LDFLAGS="-L${sysroot}/lib ${LDFLAGS} ${PLATFORM_LIBS} ${EXTRA_LDFLAGS}" \
                    PKG_CONFIG_PATH="${sysroot}/lib/pkgconfig:${sysroot}/usr/lib/pkgconfig:${sysroot}/share/pkgconfig" \
                ./configure \
                    --host=${TRIPLE} \
                    --cross-prefix=${TRIPLE}- \
                    --prefix="${sysroot}" \
                    --extra-cflags="${CFLAGS} -fPIC -O3 ${EXTRA_AUTOTOOLS_CFLAGS}" \
                    --extra-ldflags="${LDFLAGS} ${PLATFORM_LIBS}" \
                    --enable-static \
                    --disable-shared \
                    --enable-pic \
                    ${EXTRA_CONFIG} \
                    ${X264_CONF_FLAGS})

          elif [[ "$name" == "bzip2" ]]; then
            echo ">>> Building ${name} (${version}) for ${OS}-${ARCH} (bzip2 special path)..."

            # Default empty program suffix
            local PROG_SUFFIX=""

            # If targeting Windows, patch the install copy and link instructions only
            if [[ "$OS" == "windows" ]]; then
                PROG_SUFFIX=".exe"
                echo ">>> Windows target detected. Patching bzip2 Makefile install targets for .exe extensions..."
                
                # Patch cp rules to copy the .exe binaries actually generated by the compiler
                sed -i 's/cp -f bzip2 /cp -f bzip2.exe /g' "${src_dir}/Makefile"
                sed -i 's/cp -f bzip2recover /cp -f bzip2recover.exe /g' "${src_dir}/Makefile"
                
                # Patch symlink rules to target and generate .exe extensions on Windows
                sed -i 's/bin\/bzip2 /bin\/bzip2.exe /g' "${src_dir}/Makefile"
                sed -i 's/bin\/bunzip2/bin\/bunzip2.exe/g' "${src_dir}/Makefile"
                sed -i 's/bin\/bzcat/bin\/bzcat.exe/g' "${src_dir}/Makefile"
            fi

            (cd "${src_dir}" && \
              make -j $(nproc) \
                CC="${LOCAL_CC}" \
                CFLAGS="${CFLAGS} -fPIC -O3 ${EXTRA_AUTOTOOLS_CFLAGS}" \
                AR="${AR}" \
                RANLIB="${RANLIB_CMD:-ranlib}" \
                PREFIX="${sysroot}" \
                PROGNAME="bzip2${PROG_SUFFIX}" \
                install)

          else
            # Dynamically append extra libraries for font-config only
            local AUTOTOOLS_LIBS=""
            if [[ "$name" == "fontconfig" ]]; then
                AUTOTOOLS_LIBS="-lbrotlidec -lbrotlicommon"
            fi
            
            # Normal autotools path (unchanged for everything else)
            # Add -fPIC to CFLAGS/CXXFLAGS
            (cd "${src_dir}" && \
              # Inject platform-specific libtool environment variables via 'env'
              env ${CONF_ENV} LD="${ACTUAL_LD}" \
              CC="${LOCAL_CC}" \
              CXX="${LOCAL_CXX}" \
              AR="${AR}" \
              STRIP="${STRIP}" \
              RANLIB="${RANLIB_CMD:-ranlib}" \
              # Include $CFLAGS and $CXXFLAGS so the target and sysroot 
              # are passed to the compiler during feature detection and building.
              CFLAGS="${CFLAGS} -fPIC -O3 -Wall -Wextra -Wno-unused-parameter ${EXTRA_AUTOTOOLS_CFLAGS}" \
              CXXFLAGS="${CXXFLAGS} -fPIC -O3 -Wall -Wextra -Wno-unused-parameter ${EXTRA_AUTOTOOLS_CXXFLAGS}" \
              CPPFLAGS="-I${sysroot}/include ${CFLAGS}" \
              # Only pass the global LDFLAGS and PLATFORM_LIBS to avoid conflicting targets
              LDFLAGS="-L${sysroot}/lib ${LDFLAGS} ${PLATFORM_LIBS}" \
              LIBS="${AUTOTOOLS_LIBS}" \
              PKG_CONFIG_PATH="${sysroot}/lib/pkgconfig:${sysroot}/usr/lib/pkgconfig:${sysroot}/share/pkgconfig" \
              ./configure --host=${TRIPLE} --prefix="${sysroot}" \
                ${EXTRA_CONF_ARGS} \
                ${AUTO_CONF_FLAGS} \
                CC="${LOCAL_CC}" \
                CXX="${LOCAL_CXX}" \
                LD="${ACTUAL_LD}" \
                RANLIB="${RANLIB_CMD:-ranlib}" \
                CFLAGS="${CFLAGS} -fPIC -O3 -Wall -Wextra -Wno-unused-parameter ${EXTRA_AUTOTOOLS_CFLAGS}" \
                CXXFLAGS="${CXXFLAGS} -fPIC -O3 -Wall -Wextra -Wno-unused-parameter ${EXTRA_AUTOTOOLS_CXXFLAGS}" \
                LDFLAGS="-L${sysroot}/lib ${LDFLAGS} ${PLATFORM_LIBS}" \
                ${extra_flags})
          fi

          if [[ "$OS" == "darwin" ]]; then
              echo "DEBUG: Conclusively patching Libtool scripts (escaping-immune)..."
              
              find "${src_dir}" -name "libtool" -type f | while read -r lt_script; do
              echo "DEBUG: Patching $lt_script"
              
              # 1. Neutralize archive_cmds: 
              # Matches from 'archive_cmds="' up to the first '~' that contains '-keep_private_externs'
              # Replaces it with just 'archive_cmds="'
              sed -i.bak -E 's/(archive_cmds=")[^~]*-keep_private_externs[^~]*~/\1/g' "$lt_script"

              # 2. Neutralize archive_expsym_cmds: 
              # Matches the middle command between '~' symbols containing '-keep_private_externs'
              # Replaces it with a single '~'
              sed -i.bak -E 's/~[^~]*-keep_private_externs[^~]*~/~/g' "$lt_script"

              # 3. Swap the master object for the raw object list:
              # We just replace the word 'lib-master.o' with 'libobjs'. No backslashes needed.
              sed -i.bak -E 's/lib-master\.o/libobjs/g' "$lt_script"
              done
          fi

          # libffi Darwin Assembly CFI Fix
          if [[ "$name" == "libffi" && "$OS" == "darwin" ]]; then
            echo "DEBUG: Force-disabling CFI pseudo-ops in all generated libffi headers..."

            FOUND_FILES=$(find . -name "fficonfig.h" -type f)

            if [ -n "$FOUND_FILES" ]; then
                echo "$FOUND_FILES" | while read -r file; do
                    sed -i 's/#define HAVE_AS_CFI_PSEUDO_OP 1/\/* #undef HAVE_AS_CFI_PSEUDO_OP *\//' "$file"
                    echo "DEBUG: Successfully patched: $file"
                done
            else
                echo "WARNING: No fficonfig.h files found to patch in $(pwd)"
            fi
          fi

          # Capture the sysroot include path
          local SYSROOT_INC="-I${sysroot}/include"

          # Resolve "Clock skew detected" errors by normalizing timestamps
          # This ensures no file is "from the future" relative to the container clock.
          find "${src_dir}" -exec touch -t $(date +%Y%m%d%H%M.%S) {} + 2>/dev/null || true

          # Run make while explicitly passing CFLAGS. 
          # libpng uses the preprocessor to generate header files, and it 
          # doesn't always carry over the flags from ./configure.
          if [[ "$name" != "bzip2" ]]; then
            make -C "${src_dir}" -j $(nproc) CPPFLAGS="-I${sysroot}/include ${CFLAGS}"
            make -C "${src_dir}" install
          fi
          ;;
        *)
            echo "Unsupported build_type: ${build_type}"; exit 1 ;;
    esac
}

# -----------------------------------------------------------------------------
# Build all dependencies (order matters for pkg-config)
# -----------------------------------------------------------------------------
echo ">>> Building dependencies into ${sysroot}..."

build_dep "miniaudio" \
    "https://github.com/mackron/miniaudio/archive/refs/tags/${MINIAUDIO_VERSION}.tar.gz" \
    "${MINIAUDIO_VERSION}" "header-only" "miniaudio.h" ".tar.gz" "tar -xzf"

build_dep "lame" \
    "https://downloads.sourceforge.net/project/lame/lame/${LAME_VERSION}/lame-${LAME_VERSION}.tar.gz" \
    "${LAME_VERSION}" "autotools" "--disable-frontend --disable-decoder" ".tar.gz" "tar -xzf"

build_dep "x264" \
    "https://code.videolan.org/videolan/x264/-/archive/stable/x264-stable.tar.bz2" \
    "${X264_VERSION}" "autotools" "" ".tar.bz2" "tar -xjf"

build_dep "xz" \
    "https://github.com/tukaani-project/xz/releases/download/v${XZ_VERSION}/xz-${XZ_VERSION}.tar.gz" \
    "${XZ_VERSION}" "autotools" "" ".tar.gz" "tar -xzf"

build_dep "bzip2" \
    "https://sourceware.org/pub/bzip2/bzip2-${BZIP2_VERSION}.tar.gz" \
    "${BZIP2_VERSION}" "autotools" "" ".tar.gz" "tar -xzf"

build_dep "libaom" \
    "https://storage.googleapis.com/aom-releases/libaom-${LIBAOM_VERSION}.tar.gz" \
    "${LIBAOM_VERSION}" "cmake" "-DENABLE_TESTS=OFF -DENABLE_EXAMPLES=OFF -DENABLE_TOOLS=OFF -DENABLE_DOCS=OFF" ".tar.gz" "tar -xzf"

build_dep "libogg" \
    "https://downloads.xiph.org/releases/ogg/libogg-${LIBOGG_VERSION}.tar.gz" \
    "${LIBOGG_VERSION}" "autotools" "" ".tar.gz" "tar -xzf"

build_dep "libvorbis" \
    "https://downloads.xiph.org/releases/vorbis/libvorbis-${LIBVORBIS_VERSION}.tar.gz" \
    "${LIBVORBIS_VERSION}" "autotools" "" ".tar.gz" "tar -xzf"

build_dep "zlib" \
    "https://zlib.net/zlib-${ZLIB_VERSION}.tar.gz" \
    "${ZLIB_VERSION}" "cmake" "-DPKGCONFIG_INSTALL_DIR=lib/pkgconfig -DZLIB_BUILD_TESTING=OFF" ".tar.gz" "tar -xzf"

build_dep "libpng" \
    "https://sourceforge.net/projects/libpng/files/libpng16/${LIBPNG_VERSION}/libpng-${LIBPNG_VERSION}.tar.xz/download" \
    "${LIBPNG_VERSION}" "autotools" "--with-zlib-prefix=${sysroot}" ".tar.xz" "tar -xJf"

build_dep "libjpeg-turbo" \
    "https://github.com/libjpeg-turbo/libjpeg-turbo/releases/download/${LIBJPEG_TURBO_VERSION}/libjpeg-turbo-${LIBJPEG_TURBO_VERSION}.tar.gz" \
    "${LIBJPEG_TURBO_VERSION}" "cmake" "" ".tar.gz" "tar -xzf"

build_dep "libwebp" \
    "https://storage.googleapis.com/downloads.webmproject.org/releases/webp/libwebp-${LIBWEBP_VERSION}.tar.gz" \
    "${LIBWEBP_VERSION}" "cmake" "" ".tar.gz" "tar -xzf"

build_dep "tiff" \
    "https://download.osgeo.org/libtiff/tiff-${TIFF_VERSION}.tar.gz" \
    "${TIFF_VERSION}" "cmake" "-Dtiff-tools=OFF -Dtiff-tests=OFF -Dtiff-contrib=OFF -Dtiff-docs=OFF" ".tar.gz" "tar -xzf"

build_dep "brotli" \
    "https://github.com/google/brotli/archive/refs/tags/v${BROTLI_VERSION}.tar.gz" \
    "${BROTLI_VERSION}" "cmake" "-DBROTLI_SHARED_LIBS=OFF" ".tar.gz" "tar -xzf"

build_dep "libde265" \
    "https://github.com/strukturag/libde265/releases/download/v${LIBDE265_VERSION}/libde265-${LIBDE265_VERSION}.tar.gz" \
    "${LIBDE265_VERSION}" "cmake" "-DENABLE_SDL=OFF -DENABLE_DECODER=ON -DENABLE_ENCODER=OFF" ".tar.gz" "tar -xzf"

build_dep "harfbuzz" \
    "https://github.com/harfbuzz/harfbuzz/releases/download/${HARFBUZZ_VERSION}/harfbuzz-${HARFBUZZ_VERSION}.tar.xz" \
    "${HARFBUZZ_VERSION}" "meson" "-Dtests=disabled -Ddocs=disabled -Dutilities=disabled" ".tar.xz" "tar -xJf"

# Font stack for libass subtitles
build_dep "freetype" \
    "https://mirror.marwan.ma/savannah/freetype/freetype-${FREETYPE_VERSION}.tar.xz" \
    "${FREETYPE_VERSION}" "autotools" "" ".tar.xz" "tar -xJf"

build_dep "fribidi" \
    "https://github.com/fribidi/fribidi/releases/download/v${FRIBIDI_VERSION}/fribidi-${FRIBIDI_VERSION}.tar.xz" \
    "${FRIBIDI_VERSION}" "meson" "-Dtests=false -Ddocs=false" ".tar.xz" "tar -xJf"

build_dep "expat" \
    "https://github.com/libexpat/libexpat/releases/download/R_$(echo $EXPAT_VERSION | tr . _)/expat-${EXPAT_VERSION}.tar.gz" \
    "${EXPAT_VERSION}" "autotools" "" ".tar.gz" "tar -xzf"

build_dep "fontconfig" \
    "https://www.freedesktop.org/software/fontconfig/release/fontconfig-${FONTCONFIG_VERSION}.tar.xz" \
    "${FONTCONFIG_VERSION}" "autotools" "--enable-libxml2=no --disable-docs" ".tar.xz" "tar -xJf"

build_dep "libass" \
    "https://github.com/libass/libass/releases/download/${LIBASS_VERSION}/libass-${LIBASS_VERSION}.tar.gz" \
    "${LIBASS_VERSION}" "autotools" "" ".tar.gz" "tar -xzf"

OPUS_EXTRA_FLAGS=""
if [[ "$OS" == "windows" && "$ARCH" == "arm64" ]]; then
    # Disable runtime CPU detection to bypass the unsupported OS-check compile failure
    OPUS_EXTRA_FLAGS="--disable-rtcd"
fi

# Audio/Video codec libraries
build_dep "opus" \
    "https://downloads.xiph.org/releases/opus/opus-${OPUS_VERSION}.tar.gz" \
    "${OPUS_VERSION}" "autotools" "${OPUS_EXTRA_FLAGS}" ".tar.gz" "tar -xzf"

build_dep "libvpx" \
    "https://github.com/webmproject/libvpx/archive/refs/tags/v${LIBVPX_VERSION}.tar.gz" \
    "${LIBVPX_VERSION}" "autotools" "" ".tar.gz" "tar -xzf"

build_dep "x265" \
    "https://bitbucket.org/multicoreware/x265_git/downloads/x265_${X265_VERSION}.tar.gz" \
    "${X265_VERSION}" "cmake" "-DENABLE_SHARED=OFF -DHIGH_BIT_DEPTH=ON" ".tar.gz" "tar -xzf"

build_dep "dav1d" \
    "https://code.videolan.org/videolan/dav1d/-/archive/${DAV1D_VERSION}/dav1d-${DAV1D_VERSION}.tar.gz" \
    "${DAV1D_VERSION}" "meson" "-Denable_tools=false -Denable_tests=false" ".tar.gz" "tar -xzf"

echo ">>> All dependencies built successfully."

# -----------------------------------------------------------------------------
# Now build FFmpeg 8.1.2 (static only)
# -----------------------------------------------------------------------------
echo ">>> Building FFmpeg ${FFMPEG_VERSION} (static, GPL, with subtitles + image support)..."

DIRNAME="ffmpeg-${FFMPEG_VERSION}"
TARBALL="ffmpeg-${FFMPEG_VERSION}.tar.xz"

# Only download the tarball if it doesn't already exist in your cache
if [ ! -f "${WORKDIR}/${TARBALL}" ]; then
    echo ">>> Downloading clean FFmpeg source..."
    curl -L -f "https://ffmpeg.org/releases/${TARBALL}" -o "${WORKDIR}/${TARBALL}"
fi

# Always delete the old compilation directory to clear out foreign Mach-O/ELF artifacts
if [ -d "${WORKDIR}/${DIRNAME}" ]; then
    echo ">>> Wiping old build directory to prevent cross-architecture cache leakage..."
    rm -rf "${WORKDIR}/${DIRNAME}"
fi

# Fresh extraction guaranteed to be 100% clean
echo ">>> Extracting fresh workspace for ${OS}-${ARCH}..."
tar -xf "${WORKDIR}/${TARBALL}" -C "${WORKDIR}"

cd "${WORKDIR}/${DIRNAME}"

# Help pkg-config and compiler find our sysroot deps
export PKG_CONFIG_PATH="${sysroot}/lib/pkgconfig:${sysroot}/usr/lib/pkgconfig:${PKG_CONFIG_PATH}"
export PKG_CONFIG_LIBDIR="${sysroot}/lib/pkgconfig"
export PKG_CONFIG_SYSROOT_DIR="${sysroot}"
export C_INCLUDE_PATH="${sysroot}/include:${C_INCLUDE_PATH}"
export CPLUS_INCLUDE_PATH="${sysroot}/include:${CPLUS_INCLUDE_PATH}"

# Setup sanitized local flag variables to avoid polluting other builds
FFMPEG_CFLAGS="${CFLAGS}"
FFMPEG_LDFLAGS="${LDFLAGS}"

# Prepare LDFLAGS dynamically based on OS
# Note: macOS & Linux use -rpath. Windows does not support it, so we exclude it there.
if [[ "$OS" == "windows" ]]; then
    # 1. Clean the exported LDFLAGS to prevent Windows-incompatible flags 
    #    from leaking silently through the environment.
    export LDFLAGS="-L${sysroot}/lib $(echo "${LDFLAGS}" | sed 's/-Wl,-rpath,[^ ]*//g' | sed 's/-Wno-unused-command-line-argument//g') ${PLATFORM_LIBS}"
    export CFLAGS="$(echo "${CFLAGS}" | sed 's/-Wno-unused-command-line-argument//g')"
    export CXXFLAGS="$(echo "${CXXFLAGS}" | sed 's/-Wno-unused-command-line-argument//g')"

    # 2. Clean our local variables too
    FFMPEG_LDFLAGS=$(echo "${FFMPEG_LDFLAGS}" | sed 's/-Wl,-rpath,[^ ]*//g' | sed 's/-Wno-unused-command-line-argument//g')
    FFMPEG_CFLAGS=$(echo "${FFMPEG_CFLAGS}" | sed 's/-Wno-unused-command-line-argument//g')
else
    # macOS & Linux defaults (keep rpath)
    export LDFLAGS="-L${sysroot}/lib ${LDFLAGS} ${PLATFORM_LIBS}"
fi

# Map entrypoint OS/ARCH to FFmpeg values
case "${OS}-${ARCH}" in
    linux-amd64)
        FFMPEG_TARGET_OS="linux"
        FFMPEG_ARCH="x86_64"
        ;;
    linux-arm64)
        FFMPEG_TARGET_OS="linux"
        FFMPEG_ARCH="aarch64"
        ;;
    darwin-amd64)
        FFMPEG_TARGET_OS="darwin"
        FFMPEG_ARCH="x86_64"
        ;;
    darwin-arm64)
        FFMPEG_TARGET_OS="darwin"
        FFMPEG_ARCH="arm64"
        ;;
    windows-amd64)
        FFMPEG_TARGET_OS="mingw32"
        FFMPEG_ARCH="x86_64"
        ;;
    windows-arm64)
        FFMPEG_TARGET_OS="mingw32"
        FFMPEG_ARCH="aarch64"
        ;;
    *)
        FFMPEG_TARGET_OS="${OS}"
        FFMPEG_ARCH="${ARCH}"
        ;;
esac

# Resolve the correct linker & OS-specific config arguments
FFMPEG_LINKER="clang"

if [[ "$OS" == "darwin" ]]; then
    # CRITICAL: Let clang drive the linking so it resolves the macOS sysroot!
    FFMPEG_LINKER="${CC}"
elif [[ "$OS" == "windows" ]]; then
    # CRITICAL: Let clang drive the linking on Windows too so it handles compiler arguments!
    FFMPEG_LINKER="${CC}"
    
    # CRITICAL: Strip any Unix-specific dynamic linker flags out of our temporary variables
    # so they never reach the compiler test inside the Windows environment.
    FFMPEG_LINKER="${CC}"
else
    # Linux defaults
    FFMPEG_LINKER="${CC}"
fi

# DLLTOOL PATH MAPPER (Only for Windows builds)
if [[ "$OS" == "windows" ]]; then
    echo ">>> Creating a virtual dlltool symlink to resolve llvm-dlltool..."
    
    # Create a local temporary directory for our binary overrides
    mkdir -p /tmp/bin-override
    
    # Safely resolve the path of the compiler's sibling llvm-dlltool
    TOOLCHAIN_BIN_DIR=$(dirname "${CC}")
    
    # Symlink llvm-dlltool as "dlltool" inside our temp override directory
    ln -sf "${TOOLCHAIN_BIN_DIR}/llvm-dlltool" /tmp/bin-override/dlltool
    
    # Prepend this temp override directory to the execution PATH
    export PATH="/tmp/bin-override:$PATH"
fi

# The big configure line (static + requested features + subtitles)
./configure \
    --prefix="${sysroot}" \
    --enable-cross-compile \
    --target-os="${FFMPEG_TARGET_OS}" \
    --arch="${FFMPEG_ARCH}" \
    --cc="${CC}" \
    --cxx="${CXX}" \
    --as="${CC} -c -I${sysroot}/include -fPIC ${FFMPEG_CFLAGS}" \
    --ld="${FFMPEG_LINKER}" \
    --strip="${STRIP}" \
    --ar="${AR}" \
    --nm="${NM_CMD}" \
    --ranlib="${RANLIB_CMD:-ranlib}" \
    --disable-shared \
    --enable-static \
    --enable-pic \
    --enable-gpl \
    --enable-version3 \
    --enable-libx264 \
    --enable-libx265 \
    --enable-libvpx \
    --enable-libaom \
    --enable-libdav1d \
    --enable-libopus \
    --enable-libvorbis \
    --enable-libmp3lame \
    --enable-libwebp \
    --enable-libass \
    --enable-zlib \
    --enable-bzlib \
    --enable-lzma \
    --disable-rpath \
    --enable-demuxer=mp4,mov,mkv,webm,avi,flv,ts,matroska,ogg,ass,ssa \
    --enable-muxer=mp4,mov,webm,image2 \
    --enable-encoder=libx264,libx265,libvpx-vp9,libaom-av1,png,mjpeg,libwebp,tiff \
    --enable-decoder=h264,hevc,vp9,av1,aac,opus,mp3,png,jpeg,webp,tiff,ass,ssa \
    --enable-filter=scale,format,fps,thumbnail,colorspace,ass \
    --enable-parser=h264,hevc,vp9,av1,aac,opus \
    --disable-doc \
    --disable-htmlpages \
    --disable-manpages \
    --disable-debug \
    --enable-runtime-cpudetect \
    --extra-version="gotedo-impress" \
    --pkg-config=pkg-config \
    --pkg-config-flags="--static" \
    --extra-cflags="-I${sysroot}/include -fPIC ${FFMPEG_CFLAGS}" \
    --extra-ldflags="-L${sysroot}/lib ${FFMPEG_LDFLAGS}" \
    ${FFMPEG_OS_FLAGS}

# Temporarily rename the VERSION file if compiling on Windows
HAS_VERSION_RENAME=false
if [[ "$OS" == "windows" ]] && [[ -f "VERSION" ]]; then
    echo ">>> Windows compilation: Temporarily renaming VERSION file to prevent C++20 standard header collision..."
    mv VERSION VERSION.tmp
    HAS_VERSION_RENAME=true
fi

echo ">>> Compiling FFmpeg..."
make -j $(nproc) -l $(nproc)

echo ">>> Installing FFmpeg to sysroot..."
make install

# Restore the VERSION file immediately after compilation is done
if [ "$HAS_VERSION_RENAME" = true ]; then
    echo ">>> Windows compilation complete: Restoring VERSION file..."
    mv VERSION.tmp VERSION
fi

# -----------------------------------------------------------------------------
# Verification step (small test to confirm FFmpeg version via the built libraries)
# -----------------------------------------------------------------------------
echo ">>> Verification: testing built static libraries..."

# Create a tiny C program that links against libavutil and prints the version.
# This proves the static libs are correctly built, PIC, and linkable.
cat > /tmp/ffmpeg_version_test.c << 'EOF'
#include <stdio.h>
#include <libavutil/avutil.h>

int main(void) {
    printf("FFmpeg version via libavutil: %s\n", av_version_info());
    printf("avutil version: %u.%u.%u\n",
           AV_VERSION_MAJOR(avutil_version()),
           AV_VERSION_MINOR(avutil_version()),
           AV_VERSION_MICRO(avutil_version()));
    return 0;
}
EOF

# Resolve static dependency link flags dynamically using pkg-config
# The --static flag forces pkg-config to output all nested static dependencies (like -lpng, -lz, -lm, etc.)
STATIC_DEP_LIBS=$(pkg-config --static --libs libavformat libavcodec libavutil libswscale libswresample 2>/dev/null || echo "")

# If pkg-config fails or isn't fully populated yet, we fall back to a manual list
if [ -z "$STATIC_DEP_LIBS" ]; then
    STATIC_DEP_LIBS="-lavformat -lavcodec -lswscale -lswresample -lavutil ${PLATFORM_LIBS} -lz -lm -lpthread"
fi

# Only use static flag for Windows (where it is required and supported cleanly by llvm-mingw)
STATIC_LINK_FLAG=""
EXTRA_VERIFY_LIBS=""
TEST_BINARY="/tmp/ffmpeg_version_test"

if [[ "$OS" == "windows" ]]; then
    STATIC_LINK_FLAG="-static"
    TEST_BINARY="/tmp/ffmpeg_version_test.exe"
elif [[ "$OS" == "darwin" ]]; then
    # We must explicitly link the Darwin system frameworks and compiler-rt builtins
    # so the compiler driver can resolve standard symbols during static linking.
    EXTRA_VERIFY_LIBS="${PLATFORM_LIBS}"
fi

# Compile the test using static flag and the fully expanded static dependencies
echo ">>> Compiling static verification binary for ${OS}-${ARCH}..."
$CC $CFLAGS $STATIC_LINK_FLAG -o /tmp/ffmpeg_version_test \
    /tmp/ffmpeg_version_test.c \
    -I"${sysroot}/include" \
    -L"${sysroot}/lib" \
    $STATIC_DEP_LIBS \
    $EXTRA_VERIFY_LIBS

# Dynamic Execution Engine
HOST_ARCH=$(uname -m)
[ "$HOST_ARCH" = "aarch64" ] && HOST_ARCH="arm64"
[ "$HOST_ARCH" = "x86_64" ] && HOST_ARCH="amd64"

EXECUTION_SUCCESS=false

# CASE A: Native Linux execution (AMD64 on Intel host, or ARM64 on Apple Silicon Host)
if [[ "$OS" == "linux" && "$ARCH" == "$HOST_ARCH" ]]; then
    echo ">>> Running native Linux test..."
    if "$TEST_BINARY"; then
        EXECUTION_SUCCESS=true
    fi

# CASE B: Windows cross-test via Wine (utilizing the Wine packages in the Dockerfile)
elif [[ "$OS" == "windows" ]]; then
    echo ">>> Running Windows test via Wine compatibility layer..."
    
    WINE_CMD="wine"
    if [[ "$ARCH" == "amd64" || "$ARCH" == "arm64" ]]; then
        # Check standard path, otherwise fall back to full multiarch path
        if command -v wine64 &> /dev/null; then
            WINE_CMD="wine64"
        elif [ -f "/usr/lib/wine/wine64" ]; then
            WINE_CMD="/usr/lib/wine/wine64"
        fi
    fi
    
    if WINEDEBUG=-all "$WINE_CMD" "$TEST_BINARY"; then
        EXECUTION_SUCCESS=true
    fi

# CASE C: Darwin cross-target (Mach-O executable is non-runnable on Linux kernel)
else
    echo ">>> Static compilation verification successful!"
    echo ">>> (Skipping execution check: cannot run Mach-O Darwin binaries on a Linux container kernel)."
    EXECUTION_SUCCESS=true
fi

if [ "$EXECUTION_SUCCESS" = false ]; then
    echo "========================================================="
    echo " ERROR: Static verification execution failed for ${OS}-${ARCH}!"
    echo "========================================================="
    exit 1
fi

# Also show pkg-config results for key libraries (useful for your CGO build)
echo ">>> pkg-config sanity check:"
pkg-config --modversion libavcodec libavformat libavutil libswscale libass || echo "Some pkg-config files may need review"

echo "Flattening artifacts from $INSTALL_SOURCE to /output..."

# Clean up existing artifacts to ensure no stale files (like old static libs) remain.
# We use find -mindepth 1 to delete everything inside /output without 
# deleting the /output directory itself, which is a Docker mount point.
find /output -mindepth 1 -delete

# CRITICAL: We must copy the ENTIRE sysroot, not just the vips install.
# The sysroot contains the headers (include/) and pc files (lib/pkgconfig/)
# for all the static dependencies (glib, expat, etc.) which are required
# for CGO to compile headers correctly.

echo "Copying full sysroot headers and metadata..."
mkdir -p /output/bin /output/lib/pkgconfig /output/include

# Recursive Copy: Headers (Absolute Depth)
if [ -d "${sysroot}/include" ]; then
    cp -r "${sysroot}/include"/* /output/include/
fi

# Recursive Copy: Libraries and Binaries (Absolute Depth)
# This includes .so, .dylib, .dll, .a, .pc, and nested subdirectories
if [ -d "${sysroot}/lib" ]; then
    cp -r "${sysroot}/lib"/* /output/lib/
fi

if [ -d "${sysroot}/bin" ]; then
    cp -r "${sysroot}/bin"/* /output/bin/
fi

# Final Layer: Overwrite with the specific libvips build results
# This ensures that the newly built libvips.so/dll/dylib takes precedence
# over anything that might have been staged in the sysroot previously.
if [ -d "$INSTALL_SOURCE" ]; then
    cp -r "$INSTALL_SOURCE"/* /output/
fi

if [[ "$OS" == "windows" ]]; then
    echo ">>> Harvesting all LLVM-Mingw runtimes from $TRIPLE/bin..."
    
    # Path to the target-specific bin folder (e.g., x86_64-w64-mingw32/bin)
    RUNTIME_BIN_DIR="${LLVM_MINGW_PATH}/${TRIPLE}/bin"
    
    if [ -d "$RUNTIME_BIN_DIR" ]; then
        # Copy everything (*.dll) from the target bin to the output artifacts
        # This includes libc++.dll, libunwind.dll, libwinpthread-1.dll, and libomp.dll
        cp -v "$RUNTIME_BIN_DIR"/*.dll /output/bin/
    else
        echo "ERROR: Runtime bin directory not found at $RUNTIME_BIN_DIR"
        exit 1
    fi
fi

echo "Artifact flattening complete."

# Save the build checksum to prevent repetitive builds
echo "$CURRENT_CHECKSUM" > /output/build.checksum
echo "SUCCESS: Libvips built for ${OS}-${ARCH} (Fingerprint: ${CURRENT_CHECKSUM})"

# ==========================================
#           START CLEANUP STAGE
# ==========================================
echo "--- Cleaning up temporary build files ---"

# 1. Remove the temporary install destination (DESTDIR)
# This was only used to stage files for the final copy/flattening.
if [ -d "$DESTDIR" ]; then
    echo "Removing temporary install directory: $DESTDIR"
    rm -rf "$DESTDIR"
fi

# 2. Remove the Meson build directory for THIS specific OS/Arch
# We keep the source folder (vips-8.16.0) so the next run (e.g. arm64) 
# doesn't have to re-download or re-extract, but we wipe the heavy 
# binary artifacts (object files, static libs).
cd "${WORKDIR}/${DIRNAME}"
if [ -d "${BUILD_DIR}" ]; then
    echo "Removing build directory: ${BUILD_DIR}"
    rm -rf "${BUILD_DIR}"
fi

# 3. Clean dependency build directories
# Wipe build folders for all dependencies (glib, libheif, etc.)
find "${WORKDIR}" -maxdepth 2 -name "build_${OS}_${ARCH}" -type d -exec rm -rf {} +

echo "Cleanup complete. Build environment is ready for the next target."
BUILDER

RUN chmod +x /usr/local/bin/builder.sh

# -----------------------------------------------------------------------------
# Embedded Sidecar Builder Script
# -----------------------------------------------------------------------------
RUN cat <<'SIDECAR_BUILDER' > /usr/local/bin/build_sidecar.sh
#!/bin/bash
set -e

OS=${1:-linux}
ARCH=${2:-amd64}

export GOOS="$OS"
export GOARCH="$ARCH"
export CGO_ENABLED=1

# Map directly to the mounted FFmpeg libraries
FFMPEG_LIB_DIR="/ffmpeg_libs/${OS}/${ARCH}"

# Throw an error if the directory is missing or empty
if [ ! -d "$FFMPEG_LIB_DIR" ] || [ -z "$(ls -A "$FFMPEG_LIB_DIR" 2>/dev/null)" ]; then
    echo "=========================================================" >&2
    echo " ERROR: FFmpeg library directory is missing or empty!" >&2
    echo " Expected Path: $FFMPEG_LIB_DIR" >&2
    echo " Please build the FFmpeg libraries for ${OS}/${ARCH} first." >&2
    echo "=========================================================" >&2
    exit 1
fi

LLVM_MINGW_PATH="/opt/llvm-mingw/$(uname -m)"
GO_BUILDFLAGS=""
export PATH="$LLVM_MINGW_PATH/bin:${PATH}"

# 1. Determine Compiler Paths and Target Flags
case "${OS}-${ARCH}" in
    windows-amd64)
        export CC="$LLVM_MINGW_PATH/bin/x86_64-w64-mingw32-clang"
        export CXX="$LLVM_MINGW_PATH/bin/x86_64-w64-mingw32-clang++"
        EXTRA_LIBS="-lavformat -lavcodec -lavutil -lws2_32 -lbcrypt -lmfplat -lmfuuid"
        # Declare windowsgui linker flag if OS is windows to hide terminal window
        GO_BUILDFLAGS="-ldflags=-s -ldflags=-w -ldflags=-H=windowsgui"
        ;;
    windows-arm64)
        export CC="$LLVM_MINGW_PATH/bin/aarch64-w64-mingw32-clang"
        export CXX="$LLVM_MINGW_PATH/bin/aarch64-w64-mingw32-clang++"
        EXTRA_LIBS="-lavformat -lavcodec -lavutil -lws2_32 -lbcrypt -lmfplat -lmfuuid"
        # Declare windowsgui linker flag if OS is windows to hide terminal window
        GO_BUILDFLAGS="-ldflags=-s -ldflags=-w -ldflags=-H=windowsgui"
        ;;
    linux-amd64)
        export CC="/usr/bin/clang"
        export CXX="/usr/bin/clang++"
        TARGET_FLAG="-target x86_64-linux-gnu"
        export CGO_CFLAGS="$TARGET_FLAG -fPIC"
        export CGO_LDFLAGS="$TARGET_FLAG -fuse-ld=lld"
        EXTRA_LIBS="-lavformat -lavcodec -lavutil -lm -lpthread -ldl -lstdc++"
        ;;
    linux-arm64)
        export CC="/usr/bin/clang"
        export CXX="/usr/bin/clang++"
        TARGET_FLAG="-target aarch64-linux-gnu"
        export CGO_CFLAGS="$TARGET_FLAG -fPIC"
        export CGO_LDFLAGS="$TARGET_FLAG -fuse-ld=lld"
        EXTRA_LIBS="-lavformat -lavcodec -lavutil -lm -lpthread -ldl -lstdc++"
        ;;
    darwin-amd64 | darwin-arm64)
        export CC="/usr/bin/clang"
        export CXX="/usr/bin/clang++"
        # Prevent ELF strip collisions
        GO_BUILDFLAGS="-ldflags=-s -ldflags=-w"
        
        if [ "$ARCH" = "amd64" ]; then
            MAC_TARGET="${MACOSX_DEPLOYMENT_TARGET:-10.15}"
            L_ARCH="x86_64"
            TARGET_FLAG="-target x86_64-apple-macos${MAC_TARGET}"
        else
            MAC_TARGET="${MACOSX_DEPLOYMENT_TARGET:-11.0}"
            L_ARCH="arm64"
            TARGET_FLAG="-target arm64-apple-macos${MAC_TARGET}"
        fi

        mkdir -p /tmp/darwin-tools
        cat << EOF > /tmp/darwin-tools/ld
#!/bin/bash
exec /usr/bin/lld -flavor darwin -arch "${L_ARCH}" -platform_version macos "${MAC_TARGET}" "${MAC_TARGET}" "\$@"
EOF
        chmod +x /tmp/darwin-tools/ld

        # Strip wrapper script: Intercepts Go linker strip actions, mapping them to llvm-strip
        cat << EOF > /tmp/darwin-tools/strip
#!/bin/bash
/usr/bin/llvm-strip "\$@" || true
EOF
        chmod +x /tmp/darwin-tools/strip

        # Prepend the tool wrapper directory to PATH so Go resolves our strip wrapper first
        export PATH="/tmp/darwin-tools:${PATH}"

        export CGO_CFLAGS="$TARGET_FLAG --sysroot=/opt/macos-sdk"
        export CGO_LDFLAGS="$TARGET_FLAG --sysroot=/opt/macos-sdk -B/tmp/darwin-tools"
        EXTRA_LIBS="-lavformat -lavcodec -lavutil -framework CoreFoundation -framework CoreMedia -framework VideoToolbox -framework AudioToolbox -lc++ -lSystem -lresolv"
        ;;
    *)
        echo "Unsupported sidecar target: ${OS}-${ARCH}"
        exit 1
        ;;
esac

# 2. Bind paths for header and library discovery to the absolute framework mount
export CGO_CFLAGS="${CGO_CFLAGS} -I${FFMPEG_LIB_DIR}/include"
export CGO_LDFLAGS="${CGO_LDFLAGS} -L${FFMPEG_LIB_DIR}/lib ${EXTRA_LIBS}"

OUT_DIR="${SIDECAR_OUT_DIR:-/output}"
OUT_FILE="${SIDECAR_OUT_FILE:-sidecar_${OS}_${ARCH}}"

if [ "$OS" = "windows" ] && [[ "$OUT_FILE" != *.exe ]]; then
    OUT_FILE="${OUT_FILE}.exe"
fi

echo "========================================================="
echo ">>> Building Go Sidecar for $OS-$ARCH..."
echo ">>> CC Target: $CC"
echo ">>> Linker Flags: $GO_BUILDFLAGS"
echo "========================================================="

# 3. Compile
cd /src

go build $GO_BUILDFLAGS -o "${OUT_DIR}/${OUT_FILE}" main.go

echo ">>> Sidecar successfully built at ${OUT_DIR}/${OUT_FILE}"
SIDECAR_BUILDER

RUN chmod +x /usr/local/bin/build_sidecar.sh

ENTRYPOINT ["/usr/local/bin/builder.sh"]
CMD ["linux", "amd64"]