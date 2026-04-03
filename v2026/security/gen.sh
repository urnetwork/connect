#! /bin/bash
wget -i urls.txt -O - | sed "/^#.*/d" | grep -oE "\b([0-9]{1,3}\.){3}[0-9]{1,3}\b" | sort --unique > ips.txt
sed 's/\([0-9]*\)\.\([0-9]*\)\.\([0-9]*\)\.\([0-9]*\)/[4]byte{\1,\2,\3,\4}: true,/g' ips.txt > ips.go
