package main

import (
	"fmt"
	//"time"
)

func main()  {
	fmt.Println("test")
	fmt.Println(2%1024)
	fmt.Println(5 % 3 )
	fmt.Println(1 % 1024)
	fmt.Println(0 % 1024)
	arr2 := [5]int{1, 2, 3, 4, 5}
	fmt.Println(arr2)
	for i := 0; i < len(arr2)/2; i++ {
		arr2[i], arr2[len(arr2)-1-i] = arr2[len(arr2)-1-i], arr2[i]
	}

	fmt.Println(arr2)

	fmt.Print("test:")
	fmt.Println(1 << 15)

	fmt.Print("uint32(0x01020304): ")
	fmt.Println(uint32(0x01020304))
}
