// libSystem trampolines for the Darwin custody primitives.

#include "textflag.h"

TEXT libc_fsetxattr_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_fsetxattr(SB)
GLOBL	·libcFsetxattrTrampolineAddr(SB), RODATA, $8
DATA	·libcFsetxattrTrampolineAddr(SB)/8, $libc_fsetxattr_trampoline<>(SB)

TEXT libc_getattrlist_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_getattrlist(SB)
GLOBL	·libcGetattrlistTrampolineAddr(SB), RODATA, $8
DATA	·libcGetattrlistTrampolineAddr(SB)/8, $libc_getattrlist_trampoline<>(SB)
