// libSystem trampoline for the root package's descriptor attribute writes.

#include "textflag.h"

TEXT libc_fsetxattr_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_fsetxattr(SB)
GLOBL	·libcFsetxattrTrampolineAddr(SB), RODATA, $8
DATA	·libcFsetxattrTrampolineAddr(SB)/8, $libc_fsetxattr_trampoline<>(SB)
