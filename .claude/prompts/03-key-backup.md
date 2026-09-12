Implement Matrix key backup APIs in the account database.

- new KeyBackup directory under AccountsDatabase
    - types.KeyBackupVersion (version, algo, authdata)
    - types.KeyBackupVersionWithMeta (types.KeyBackupVersion, count, updatedVersionstamp)
    - KeyBackup.userVersions subspace tuple.Tuple{userid, versionstamp} -> tuple.Tuple{algo, authdata}
        - combine version from key and algo/authdata -> types.KeyBackupVersion
        - version as string becomes the API version passed to client
    - KeyBackup.userVersionsMeta subspace tuple.Tuple{userid, versionstamp} -> tuple.Tuple{count, updatedVersionstamp}
        - read/update this on add/remove keys to backup
        - use updatedVersion as etag version
    - KeyBackup.keys subspace (userid, backup-versionstamp, roomid, sessionid)
    - methods: KeyBackup.TxnStoreVersion, KeyBackup.TxnStoreKeys, KeyBackup.TxnGetVersion, KeyBackup.TxnGetVersionWithMeta, etc...
- add all the various matrix key backup endpoints under new client routes file
    - return/bump updatedVersionstamp as write version etag
- you can get the matrix key backup spec online: https://raw.githubusercontent.com/matrix-org/matrix-spec/main/data/api/client-server/key_backup.yaml

Explore the codebase with a few agents and come up with a plan to implement the above changes.
