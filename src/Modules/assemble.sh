#!/bin/sh
set -eu
DIST=$1
PATCHELF=/src/.venv/bin/patchelf
SITE=/src/.venv/lib/python3.14/site-packages

test -x "$DIST/module-service"
test -z "$(find "$DIST" -name '*.py' -o -name '*.pyc' -o -name 'python3*')"
if $PATCHELF --print-needed "$DIST/module-service" | grep -q libpython; then
  echo "libpython is linked dynamically"
  exit 1
fi
rm -f "$DIST"/ld-musl-*.so.1
find "$DIST" -name '*.so*' -exec strip --strip-unneeded {} +
strip "$DIST/module-service"

nm -D --defined-only "$DIST/module-service" | awk '{ print $3 }' | sort -u > /tmp/exported
find "$DIST" -name '*.so' -exec nm -D --undefined-only {} + | awk '{ print $2 }' | grep -E '^_?Py' | sort -u > /tmp/imported
comm -23 /tmp/imported /tmp/exported > /tmp/unresolved
if [ -s /tmp/unresolved ]; then
  echo "Python C-API symbols imported by extension modules but not exported by the executable:"
  cat /tmp/unresolved
  exit 1
fi
echo "exported C-API symbols: $(grep -cE '^_?Py' /tmp/exported); imported by extension modules: $(wc -l < /tmp/imported)"

needed() {
  $PATCHELF --print-needed "$1" | grep -vE '^(libc\.so|libc\.musl-.*|ld-musl-.*)$' || true
}
elves() {
  echo "$DIST/module-service"
  find "$DIST" -name '*.so*'
}

copied=1
while [ "$copied" = 1 ]; do
  copied=0
  for f in $(elves); do
    for n in $(needed "$f"); do
      [ -e "$DIST/$n" ] && continue
      src=""
      for d in /usr/lib /lib; do
        [ -e "$d/$n" ] && src="$d/$n" && break
      done
      [ -n "$src" ] || { echo "missing shared library $n needed by $f"; exit 1; }
      cp -L "$src" "$DIST/$n"
      echo "added $n from $src"
      copied=1
    done
  done
done
for f in $(elves); do
  for n in $(needed "$f"); do
    [ -e "$DIST/$n" ] || { echo "unresolved $n needed by $f"; exit 1; }
  done
done

pkgs="musl python3 ca-certificates-bundle"
for f in "$DIST"/*.so*; do
  n=$(basename "$f")
  for d in /usr/lib /lib; do
    if [ -e "$d/$n" ]; then
      pkgs="$pkgs $(apk info --who-owns "$d/$n" | sed -n 's/.* is owned by \(.*\)-[0-9][^-]*-r[0-9]*$/\1/p')"
      break
    fi
  done
done
install -d /skel/lib/apk/db /skel/etc /skel/app/site-packages
install -d -m 1777 /skel/tmp
awk -v list="$pkgs" '
  BEGIN { RS = ""; ORS = "\n\n"; n = split(list, a, " "); for (i = 1; i <= n; i++) want[a[i]] = 1 }
  { p = ""; m = split($0, l, "\n"); for (i = 1; i <= m; i++) if (substr(l[i], 1, 2) == "P:") p = substr(l[i], 3); if (p in want) print }
' /lib/apk/db/installed > /skel/lib/apk/db/installed
cp /etc/alpine-release /etc/os-release /skel/etc/
for d in "$SITE"/*.dist-info; do
  case "$(basename "$d")" in nuitka-*|patchelf-*) continue ;; esac
  install -D -m 644 "$d/METADATA" "/skel/app/site-packages/$(basename "$d")/METADATA"
done
echo "packages: $(grep -c '^P:' /skel/lib/apk/db/installed) ($pkgs)"
echo "python packages: $(ls /skel/app/site-packages | wc -l); dist: $(du -sh "$DIST" | cut -f1)"
