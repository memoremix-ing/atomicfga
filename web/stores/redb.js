// A store adapter over the redb module (rust/web/redb), a WebAssembly module
// separate from the evaluator: values are copied between the two here.
// Persisted to an OPFS file (in a worker), or in memory.

const encoder = new TextEncoder();

export class RedbStore {
  #e;
  #handle = null;

  /**
   * Opens the store. With `opfsFile` (a name in the origin's private file
   * system), the database lives in that file; without it, in memory.
   */
  static async open(source, { opfsFile } = {}) {
    const store = new RedbStore();
    let handle = null;
    if (opfsFile) {
      const root = await navigator.storage.getDirectory();
      const file = await root.getFileHandle(opfsFile, { create: true });
      handle = await file.createSyncAccessHandle();
    }
    const bytes = source instanceof Uint8Array || source instanceof ArrayBuffer ? source : await (await fetch(source)).arrayBuffer();
    const view = (p, n) => new Uint8Array(store.#e.memory.buffer, p, n);
    const opfs = {
      opfs_len: () => handle.getSize(),
      opfs_read: (offset, dst, len) => {
        let done = 0;
        while (done < len) {
          const n = handle.read(view(dst + done, len - done), { at: offset + done });
          if (n <= 0) return 1;
          done += n;
        }
        return 0;
      },
      opfs_write: (offset, src, len) => {
        let done = 0;
        while (done < len) {
          const n = handle.write(view(src + done, len - done), { at: offset + done });
          if (n <= 0) return 1;
          done += n;
        }
        return 0;
      },
      opfs_set_len: (len) => (handle.truncate(len), 0),
      opfs_sync: () => (handle.flush(), 0),
    };
    const { instance } = await WebAssembly.instantiate(bytes, { opfs });
    store.#e = instance.exports;
    store.#handle = handle;
    store.#check(store.#e.open(handle ? 1 : 0));
    return store;
  }

  /** Closes the database and its file. */
  close() {
    this.#e.close();
    this.#handle?.close();
    this.#handle = null;
  }

  #check(n) {
    if (n < -1) {
      const len = -2 - n;
      const p = this.#e.result_ptr();
      throw new Error(new TextDecoder().decode(new Uint8Array(this.#e.memory.buffer, p, len)));
    }
    return n;
  }

  // Calls `f` with (pointer, length) pairs for `args` in the module's memory.
  #call(args, f) {
    const e = this.#e;
    const bufs = args.map((x) => (typeof x === "string" ? encoder.encode(x) : x));
    const ptrs = bufs.map((b) => {
      const p = e.alloc(b.length);
      new Uint8Array(e.memory.buffer, p, b.length).set(b);
      return p;
    });
    try {
      return this.#check(f(...bufs.flatMap((b, i) => [ptrs[i], b.length])));
    } finally {
      bufs.forEach((b, i) => e.dealloc(ptrs[i], b.length));
    }
  }

  #bytes(n) {
    if (n === -1) return null;
    const p = this.#e.result_ptr();
    return new Uint8Array(this.#e.memory.buffer, p, n).slice();
  }

  begin(write) {
    this.#check(write ? this.#e.begin_write() : this.#e.begin_read());
  }
  commit() {
    this.#check(this.#e.commit());
  }
  abort() {
    this.#check(this.#e.abort());
  }
  tupleGet(store, object, relation, user) {
    return this.#bytes(this.#call([store, object, relation, user], this.#e.tuple_get));
  }
  tupleScan(store, object, relation) {
    return this.#bytes(this.#call([store, object, relation], this.#e.tuple_scan));
  }
  tuplePut(store, object, relation, user, value) {
    this.#call([store, object, relation, user, value], this.#e.tuple_put);
  }
  tupleDelete(store, object, relation, user) {
    this.#call([store, object, relation, user], this.#e.tuple_delete);
  }
  modelGet(store, id) {
    return this.#bytes(this.#call([store, id], this.#e.model_get));
  }
  modelLatest(store) {
    return this.#bytes(this.#call([store], this.#e.model_latest));
  }
  modelPut(store, id, model) {
    this.#call([store, id, model], this.#e.model_put);
  }
}
