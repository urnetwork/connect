# Frogeye feed validation

On November 13, 2026 the official Frogeye lists contained 4,922 first-party
and 9,591 multiparty domains, below the historical 8,000/10,000 floors.
Both raw lists exactly matched the domain payloads of the corresponding
official hosts variants. Those variants declared the same output counts,
generation at `2026-11-09T00:00:50+01:00`, oldest records from November 8,
3,219,361 source subdomains and 2,843,950 source DNS records. This established
complete upstream output rather than a parser format failure. The reason for
the upstream count reduction was not independently established.

The operator approved recalibrating the floors to 4,000/8,000 together with
mandatory metadata validation. The generator now consumes the official hosts
variants and rejects missing or duplicate required metadata, incorrect title
or variant, malformed or duplicate domains, non-null hosts targets, mismatched
declared output counts, source subdomain or DNS volumes below one million,
generation or oldest-record timestamps older than 14 days or over 24 hours
in the future, and oldest records newer than generation. These checks fail
the normal release; they do not silently omit feeds. Existing total counts,
public-suffix rejection, IP poison guards and size limits remain in force.

The source files and format are documented upstream:

- [First-party hosts](https://hostfiles.frogeye.fr/firstparty-trackers-hosts.txt)
- [Multiparty hosts](https://hostfiles.frogeye.fr/multiparty-trackers-hosts.txt)
- [Export script](https://git.frogeye.fr/geoffrey/eulaurarien/src/branch/master/export_lists.sh)
- [Pruning script](https://git.frogeye.fr/geoffrey/eulaurarien/src/branch/master/prune.sh)

The upstream pipeline prunes old records before exporting. That mechanism is
consistent with variable list volume; it does not prove the cause of this
particular reduction. Future unexpected reductions require investigation.

# Lossless fingerprint packing

Growth in the other feeds brought the live aggregate to 183,430 unique host
fingerprints, requiring 1,100,612 bytes with the previous six bytes per record.
That exceeded the existing one MiB embedded-data limit. The current format
stores the leading fingerprint byte as a bucket number: 257 four-byte offsets
delimit the 256 buckets, and each record stores its remaining five bytes.
This retains every fingerprint bit, all feeds and the original size limit.
For that aggregate it uses 918,210 embedded bytes including the pepper.
Lookup searches only within the selected bucket and allocates no heap memory
on the ASCII path.
