package monitor

// mimirPublisherSocketOwnersAwk is appended to a bounded ss-row reducer.
// publisherOwnsSocket returns 1 for the exact live PID, 0 for fully observed
// other owners, and -1 for unavailable or malformed ownership. Process names
// are opaque quoted strings, never PID evidence and never reducer output.
const mimirPublisherSocketOwnersAwk = `
function publisherSocketSkipSpace(line, position) {
  while (position <= length(line) && substr(line,position,1) ~ /^[[:space:]]$/) position++
  return position
}
function publisherOwnsSocket(line, pid, position, limit, tuples, owned, character, start, ownerPid, descriptor) {
  limit=length(line)
  if (limit > 4096 || line ~ /[\r\n]/ || length(pid) > 20 || pid !~ /^[1-9][0-9]*$/) return -1
  position=index(line,"users:")
  if (position == 0 || (position > 1 && substr(line,position-1,1) !~ /^[[:space:]]$/)) return -1
  position=publisherSocketSkipSpace(line,position+6)
  if (substr(line,position,1) != "(") return -1
  position++
  while (position <= limit) {
    if (++tuples > 64) return -1
    position=publisherSocketSkipSpace(line,position)
    if (substr(line,position,1) != "(") return -1
    position=publisherSocketSkipSpace(line,position+1)
    if (substr(line,position,1) != "\"") return -1
    position++
    while (position <= limit) {
      character=substr(line,position,1)
      if (character == "\\") {
        position+=2
        if (position > limit+1) return -1
        continue
      }
      if (character == "\"") break
      position++
    }
    if (position > limit) return -1
    position=publisherSocketSkipSpace(line,position+1)
    if (substr(line,position,1) != ",") return -1
    position=publisherSocketSkipSpace(line,position+1)
    if (substr(line,position,4) != "pid=") return -1
    position+=4
    start=position
    while (position <= limit && substr(line,position,1) ~ /^[0-9]$/) position++
    ownerPid=substr(line,start,position-start)
    if (length(ownerPid) > 20 || ownerPid !~ /^[1-9][0-9]*$/) return -1
    position=publisherSocketSkipSpace(line,position)
    if (substr(line,position,1) != ",") return -1
    position=publisherSocketSkipSpace(line,position+1)
    if (substr(line,position,3) != "fd=") return -1
    position+=3
    start=position
    while (position <= limit && substr(line,position,1) ~ /^[0-9]$/) position++
    descriptor=substr(line,start,position-start)
    if (length(descriptor) > 20 || descriptor !~ /^[0-9]+$/ || (length(descriptor) > 1 && substr(descriptor,1,1) == "0")) return -1
    position=publisherSocketSkipSpace(line,position)
    if (substr(line,position,1) != ")") return -1
    if (("pid:" ownerPid) == ("pid:" pid)) owned=1
    position=publisherSocketSkipSpace(line,position+1)
    character=substr(line,position,1)
    if (character == ",") {position++; continue}
    if (character != ")") return -1
    position=publisherSocketSkipSpace(line,position+1)
    if (position <= limit) return -1
    return owned+0
  }
  return -1
}
`
