"""Post-run, private, lossless evidence COPY; never deletes/rewrites source.

No live-run compression: run only after observer/cleanup have stopped. The
verified copy does not free source capacity. No network or cloud operations.
"""
import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import tarfile

GIB=1024**3

def digest(stream):
    h=hashlib.sha256()
    for block in iter(lambda:stream.read(2**20),b''):h.update(block)
    return h.hexdigest()


def signature(path):
    s=path.stat()
    return (s.st_dev,s.st_ino,s.st_size,s.st_mtime_ns,s.st_ctime_ns)


def archive_copy(source, destination, reserve_bytes=80*GIB):
    source=Path(source).resolve();destination=Path(destination).absolute()
    if not source.is_dir() or destination.resolve().is_relative_to(source):
        raise ValueError('archive must be outside source directory')
    if destination.exists() or destination.with_suffix(destination.suffix+'.index.json').exists():
        raise FileExistsError('archive or index already exists')
    files=sorted(p for p in source.rglob('*') if p.is_file())
    if any(p.is_symlink() for p in source.rglob('*')):raise ValueError('symlink in source; explicit review required')
    expected={str(p.relative_to(source)):signature(p) for p in files}
    destination.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
    destination=destination.parent.resolve()/destination.name
    # Hard links and compression ratios are not assumed for free-space admission.
    free=lambda:os.statvfs(destination.parent).f_bavail*os.statvfs(destination.parent).f_frsize
    if free()<=reserve_bytes+sum(v[2] for v in expected.values()):raise ValueError('insufficient safe archive headroom')
    temp=destination.with_name(destination.name+'.partial');index=[]
    # Exclusive creation, private even under a permissive caller umask.
    fd=os.open(temp,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(fd,'wb') as raw:
        with gzip.GzipFile(fileobj=raw,mode='wb',compresslevel=1,mtime=0) as zipped:
            with tarfile.open(fileobj=zipped,mode='w|') as tar:
                for path in files:
                    name=str(path.relative_to(source))
                    if signature(path)!=expected[name] or free()<=reserve_bytes:raise ValueError('source changed or disk reserve reached')
                    with path.open('rb') as f:hashed=digest(f)
                    with path.open('rb') as f:
                        info=tar.gettarinfo(str(path),arcname=name);tar.addfile(info,f)
                    if signature(path)!=expected[name]:raise ValueError('source changed during copy')
                    index.append(dict(path=name,bytes=expected[name][2],sha256=hashed))
        raw.flush();os.fsync(raw.fileno())
    actual={}
    with tarfile.open(temp,'r|gz') as tar:
        for item in tar:
            if not item.isfile() or item.name in actual:raise ValueError('invalid archive member')
            with tar.extractfile(item) as f:actual[item.name]=dict(bytes=item.size,sha256=digest(f))
    assert actual=={v['path']:dict(bytes=v['bytes'],sha256=v['sha256']) for v in index},'lossless roundtrip verification failed'
    if {str(p.relative_to(source)) for p in source.rglob('*') if p.is_file()}!=set(expected):raise ValueError('source file set changed')
    for path in files:
        if signature(path)!=expected[str(path.relative_to(source))]:raise ValueError('source changed before final commit')
    with temp.open('rb') as f:archive_hash=digest(f)
    # Exclusive hard-link commit: never overwrite an existing archive.
    os.link(temp,destination);temp.unlink()
    report=dict(source=str(source),archive=str(destination),archive_sha256=archive_hash,
        archive_bytes=destination.stat().st_size,source_logical_bytes=sum(v['bytes'] for v in index),file_count=len(index),
        every_file_roundtrip_sha256_verified=True,originals_unchanged=True,sources_deleted=False,
        security='PRIVATE FULL EVIDENCE COPY; may contain confidential operational receipts; never commit/upload publicly',files=index)
    out=destination.with_suffix(destination.suffix+'.index.json')
    fd=os.open(out,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(fd,'w') as f:json.dump(report,f,indent=2);f.flush();os.fsync(f.fileno())
    fd=os.open(destination.parent,os.O_RDONLY)
    try:os.fsync(fd)
    finally:os.close(fd)
    return report


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--source',type=Path,required=True);p.add_argument('--archive',type=Path,required=True)
    a=p.parse_args();r=archive_copy(a.source,a.archive)
    print(json.dumps({k:v for k,v in r.items() if k!='files'},indent=2))

if __name__=='__main__':main()
