cmake_minimum_required(VERSION 3.16)

set(PROJECT_VER "0.2.0")
include($ENV{IDF_PATH}/tools/cmake/project.cmake)
project(echoear)

# Bound recursive parsing on the small HTTP and conversation task stacks.
idf_component_get_property(json_lib json COMPONENT_LIB)
target_compile_definitions(${json_lib} PRIVATE CJSON_NESTING_LIMIT=16)
