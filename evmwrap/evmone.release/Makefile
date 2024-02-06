OBJS = \
   advanced_analysis.o \
   advanced_execution.o \
   advanced_instructions.o \
   baseline.o \
   baseline_instruction_table.o \
   eof.o \
   instructions_calls.o \
   instructions_storage.o \
   tracing.o \
   vm.o \


all : ${OBJS}

%.o : %.cpp
	g++ -O3 -static -std=c++20 -c -I ./include -I ../evmc/include/ -I ../intx/include/ -I ../keccak/include -o $@ -c $<

clean:
	rm *.o


