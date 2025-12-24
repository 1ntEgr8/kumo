import argparse
import time
from pathlib import Path
from dataclasses import dataclass
from typing import List, Dict, Set
import pandas as pd
from tqdm import tqdm
import math
from collections import defaultdict
@dataclass
class InterfaceTypeInfo:
    name: str
    methods: Set[str]
    implementations: Set[str]
@dataclass
class ConcreteTypeInfo:
    name: str
    methods: Set[str]
    implements: Set[str]
def implements_(ctype_methods: Set[str], iface_methods: Set[str]):
    return (iface_methods&ctype_methods)==iface_methods
class Simulator:
    def __init__(self):
        pass
    def replay(self, df):
        relations = []
        for record in tqdm(df.to_records()):
            if record['kind'] == 'concrete':
                C = record['type_name']
                methods = set(record['methods'])
                implements = self.handle_ctype(C, methods)
                self._cinfo[C] = ConcreteTypeInfo(name=C, implements=implements, methods=methods)
            if record['kind'] == 'interface':
                I = record['type_name']
                methods = set(record['methods'])
                implementations = self.handle_iface(I, methods)
                self._iinfo[I] = InterfaceTypeInfo(name=I, implementations=implementations, methods=methods)
        relations = []
        for cinfo in self._cinfo.values():
            relations.append({"concrete_type": cinfo.name, "implements": sorted(list(cinfo.implements))})
        return relations
    def reset(self):
        raise NotImplementedError
    def handle_iface(self, I, methods):
        raise NotImplementedError
    def handle_ctype(self, C, methods):
        raise NotImplementedError
class NaiveSimulator(Simulator):
    def __init__(self):
        self.reset();
    def reset(self):
        self._iinfo: Dict[str, InterfaceTypeInfo] = {}
        self._cinfo: Dict[str, ConcreteTypeInfo] = {}
    def handle_ctype(self, C, methods):
        implements=set()
        for iinfo in self._iinfo.values():
            if implements_(methods, iinfo.methods):
                implements.add(iinfo.name)
                iinfo.implementations.add(C)
        return implements
    def handle_iface(self, I, methods):
        implementations=set()
        for cinfo in self._cinfo.values():
            if implements_(cinfo.methods, methods):
                implementations.add(cinfo.name)
                cinfo.implements.add(I)
        return implementations
class KumoSimulator(Simulator):
    def __init__(self):
        self.reset()
    def reset(self):
        self._iinfo: Dict[str, InterfaceTypeInfo] = {}
        self._cinfo: Dict[str, ConcreteTypeInfo] = {}
        self._MC = {}
        self._MI = {}
        self._method_counts = defaultdict(int)
    def handle_ctype(self, C, methods):
        implements = set()
        for m in methods:
            if m not in self._MC:
                self._MC[m] = []
            self._MC[m].append(C)
        cands = []
        for m in methods:
            cands.extend(self._MI.get(m, []))
        for I in cands:
            iinfo = self._iinfo[I]
            if implements_(methods, iinfo.methods):
                implements.add(iinfo.name)
                iinfo.implementations.add(C)
        return implements
    def handle_iface(self, I, methods):
        # update method counts
        for m in methods:
            self._method_counts[m] += 1 
        _, _, rarestm = min([(len(m), self._method_counts[m], m) for m in methods], key=lambda x: (x[0], x[1]))    
        # update interface method index
        if rarestm not in self._MI:
            self._MI[rarestm] = []
        self._MI[rarestm].append(I)
        # update facts
        for m in methods:
            if m not in self._MC:
                return set()
        _, bestm = min([(len(self._MC), m) for m in methods], key=lambda x: x[0])
        implementations = set()
        for C in self._MC[bestm]:
            cinfo = self._cinfo[C]
            if implements_(cinfo.methods, methods):
                implementations.add(cinfo.name)
                cinfo.implements.add(I)
        return implementations
def validate(actual_relations, expected_relations):
    for r in tqdm(actual_relations):
        x = set(r['implements'])
        tmp = expected_relations[expected_relations['concrete_type']==r['concrete_type']].iloc[0]['implements']
        y = set() if tmp is None else set(tmp)
        assert x == y, f"Failed validation! concrete type: {r['concrete_type']}. Actual: {x}, Expected: {y}"
def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--discovery-jsonl", required=True, type=Path)
    parser.add_argument("--relations-jsonl", required=True, type=Path)
    args = parser.parse_args()
    df = pd.read_json(args.discovery_jsonl, lines=True)
    rf = pd.read_json(args.relations_jsonl, lines=True)
    print("replaying the trace (using naive)")
    sim = NaiveSimulator()
    start_time = time.perf_counter()
    relations_0 = sim.replay(df)
    end_time = time.perf_counter()
    print(f"finished in {end_time - start_time:.2f}s")
    print("replaying the trace (using kumo)")
    ksim = KumoSimulator()
    start_time = time.perf_counter()
    relations_1 = ksim.replay(df)
    end_time = time.perf_counter()
    print(f"finished in {end_time - start_time:.2f}s")
    if relations_0 == relations_1:
        print("Both approaches produced the same final result!")
    else:
        print("Discrepancy detected!")
        import pdb
        pdb.set_trace()
    # print("validating result")
    # try:
    #     validate(relations, rf)
    # except AssertionError as e:
    #     print(e)
    #     import traceback
    #     traceback.print_exc()
    #     print("Putting you into an interactive debug session")
    #     import pdb
    #     pdb.set_trace()
if __name__ == "__main__":
    main()
